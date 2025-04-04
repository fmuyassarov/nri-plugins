// Copyright 2022 Intel Corporation. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package balloons

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/topology"
	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

// cpuTreeNode is a node in the CPU tree.
type cpuTreeNode struct {
	name     string
	level    CPUTopologyLevel
	parent   *cpuTreeNode
	children []*cpuTreeNode
	cpus     cpuset.CPUSet // union of CPUs of child nodes
	sys      system.System
}

// cpuTreeNodeAttributes contains various attributes of a CPU tree
// node. When allocating or releasing CPUs, all CPU tree nodes in
// which allocating/releasing could be possible are stored to the same
// slice with these attributes. The attributes contain all necessary
// information for comparing which nodes are the best choices for
// allocating/releasing, thus traversing the tree is not needed in the
// comparison phase.
type cpuTreeNodeAttributes struct {
	t                *cpuTreeNode
	depth            int
	currentCpus      cpuset.CPUSet
	freeCpus         cpuset.CPUSet
	currentCpuCount  int
	currentCpuCounts []int
	freeCpuCount     int
	freeCpuCounts    []int
}

// cpuTreeAllocator allocates CPUs from the branch of a CPU tree
// where the "root" node is the topmost CPU of the branch.
type cpuTreeAllocator struct {
	options           cpuTreeAllocatorOptions
	root              *cpuTreeNode
	cacheCloseCpuSets map[string][]cpuset.CPUSet
}

// cpuTreeAllocatorOptions contains parameters for the CPU allocator
// that that selects CPUs from a CPU tree.
type cpuTreeAllocatorOptions struct {
	// topologyBalancing true prefers allocating from branches
	// with most free CPUs (spread allocations), while false is
	// the opposite (packed allocations).
	topologyBalancing           bool
	preferSpreadOnPhysicalCores bool
	preferCloseToDevices        []string
	preferFarFromDevices        []string
	virtDevCpusets              map[string][]cpuset.CPUSet
	deviceUpdateOnEveryCpu      func(cpuset.CPUSet)
}

var emptyCpuSet = cpuset.New()

// String returns string representation of a CPU tree node.
func (t *cpuTreeNode) String() string {
	if len(t.children) == 0 {
		return t.name
	}
	return fmt.Sprintf("%s%v", t.name, t.children)
}

func (t *cpuTreeNode) PrettyPrint() string {
	origDepth := t.Depth()
	lines := []string{}
	if err := t.DepthFirstWalk(func(tn *cpuTreeNode) error {
		lines = append(lines,
			fmt.Sprintf("%s%s: %q cpus: %s",
				strings.Repeat(" ", (tn.Depth()-origDepth)*4),
				tn.level, tn.name, tn.cpus))
		return nil
	}); err != nil && err != WalkSkipChildren && err != WalkStop {
		log.Warnf("failed to walk CPU tree: %v", err)
	}
	return strings.Join(lines, "\n")
}

func (t *cpuTreeNode) system() system.System {
	if t.sys != nil || t.parent == nil {
		return t.sys
	}
	return t.parent.system()
}

// String returns cpuTreeNodeAttributes as a string.
func (tna cpuTreeNodeAttributes) String() string {
	return fmt.Sprintf("%s{%d,%v,%d,%d}", tna.t.name, tna.depth,
		tna.currentCpuCounts,
		tna.freeCpuCount, tna.freeCpuCounts)
}

// NewCpuTree returns a named CPU tree node.
func NewCpuTree(name string) *cpuTreeNode {
	return &cpuTreeNode{
		name: name,
		cpus: cpuset.New(),
	}
}

func (t *cpuTreeNode) CopyTree() *cpuTreeNode {
	newNode := t.CopyNode()
	newNode.children = make([]*cpuTreeNode, 0, len(t.children))
	for _, child := range t.children {
		newNode.AddChild(child.CopyTree())
	}
	return newNode
}

func (t *cpuTreeNode) CopyNode() *cpuTreeNode {
	newNode := cpuTreeNode{
		name:     t.name,
		level:    t.level,
		parent:   t.parent,
		children: t.children,
		cpus:     t.cpus,
	}
	return &newNode
}

// Depth returns the distance from the root node.
func (t *cpuTreeNode) Depth() int {
	if t.parent == nil {
		return 0
	}
	return t.parent.Depth() + 1
}

// AddChild adds new child node to a CPU tree node.
func (t *cpuTreeNode) AddChild(child *cpuTreeNode) {
	child.parent = t
	t.children = append(t.children, child)
}

// AddCpus adds CPUs to a CPU tree node and all its parents.
func (t *cpuTreeNode) AddCpus(cpus cpuset.CPUSet) {
	t.cpus = t.cpus.Union(cpus)
	if t.parent != nil {
		t.parent.AddCpus(cpus)
	}
}

// Cpus returns CPUs of a CPU tree node.
func (t *cpuTreeNode) Cpus() cpuset.CPUSet {
	return t.cpus
}

// SiblingIndex returns the index of this node among its parents
// children. Returns -1 for the root node, -2 if this node is not
// listed among the children of its parent.
func (t *cpuTreeNode) SiblingIndex() int {
	if t.parent == nil {
		return -1
	}
	for idx, child := range t.parent.children {
		if child == t {
			return idx
		}
	}
	return -2
}

func (t *cpuTreeNode) FindLeafWithCpu(cpu int) *cpuTreeNode {
	var found *cpuTreeNode
	if err := t.DepthFirstWalk(func(tn *cpuTreeNode) error {
		if len(tn.children) > 0 {
			return nil
		}
		for _, cpuHere := range tn.cpus.List() {
			if cpu == cpuHere {
				found = tn
				return WalkStop
			}
		}
		return nil // not found here, no more children to search
	}); err != nil && err != WalkSkipChildren && err != WalkStop {
		log.Warnf("failed to walk CPU tree: %v", err)
	}
	return found
}

// WalkSkipChildren error returned from a DepthFirstWalk handler
// prevents walking deeper in the tree. The caller of the
// DepthFirstWalk will get no error.
var WalkSkipChildren error = errors.New("skip children")

// WalkStop error returned from a DepthFirstWalk handler stops the
// walk altogether. The caller of the DepthFirstWalk will get the
// WalkStop error.
var WalkStop error = errors.New("stop")

// DepthFirstWalk walks through nodes in a CPU tree. Every node is
// passed to the handler callback that controls next step by
// returning:
// - nil: continue walking to the next node
// - WalkSkipChildren: continue to the next node but skip children of this node
// - WalkStop: stop walking.
func (t *cpuTreeNode) DepthFirstWalk(handler func(*cpuTreeNode) error) error {
	if err := handler(t); err != nil {
		if err == WalkSkipChildren {
			return nil
		}
		return err
	}
	for _, child := range t.children {
		if err := child.DepthFirstWalk(handler); err != nil {
			return err
		}
	}
	return nil
}

// CpuLocations returns a slice where each element contains names of
// topology elements over which a set of CPUs spans. Example:
// systemNode.CpuLocations(cpuset:0,99) = [["system"],["p0", "p1"], ["p0d0", "p1d0"], ...]
func (t *cpuTreeNode) CpuLocations(cpus cpuset.CPUSet) [][]string {
	names := make([][]string, int(CPUTopologyLevelCount)-t.level.Value())
	if err := t.DepthFirstWalk(func(tn *cpuTreeNode) error {
		if tn.cpus.Intersection(cpus).Size() == 0 {
			return WalkSkipChildren
		}
		levelIndex := tn.level.Value() - t.level.Value()
		names[levelIndex] = append(names[levelIndex], tn.name)
		return nil
	}); err != nil && err != WalkSkipChildren && err != WalkStop {
		log.Warnf("failed to walk CPU tree: %v", err)
	}
	return names
}

// NewCpuTreeFromSystem returns the root node of the topology tree
// constructed from the underlying system.
func NewCpuTreeFromSystem() (*cpuTreeNode, error) {
	sys, err := system.DiscoverSystem(system.DiscoverCPUTopology | system.DiscoverCache)
	if err != nil {
		return nil, err
	}
	// TODO: split deep nested loops into functions
	sysTree := NewCpuTree("system")
	sysTree.sys = sys
	sysTree.level = CPUTopologyLevelSystem
	for _, packageID := range sys.PackageIDs() {
		packageTree := NewCpuTree(fmt.Sprintf("p%d", packageID))
		packageTree.level = CPUTopologyLevelPackage
		cpuPackage := sys.Package(packageID)
		sysTree.AddChild(packageTree)
		for _, dieID := range cpuPackage.DieIDs() {
			dieTree := NewCpuTree(fmt.Sprintf("%sd%d", packageTree.name, dieID))
			dieTree.level = CPUTopologyLevelDie
			packageTree.AddChild(dieTree)
			for _, nodeID := range cpuPackage.DieNodeIDs(dieID) {
				nodeTree := NewCpuTree(fmt.Sprintf("%sn%d", dieTree.name, nodeID))
				nodeTree.level = CPUTopologyLevelNuma
				dieTree.AddChild(nodeTree)
				node := sys.Node(nodeID)

				// Find all level 2 caches (l2c) shared by CPUs of this node.
				l2cs := map[*system.Cache]struct{}{}
				for _, cpuID := range node.CPUSet().List() {
					for _, cache := range sys.CPU(cpuID).GetCachesByLevel(2) {
						l2cs[cache] = struct{}{}
					}
				}

				for cache := range l2cs {
					l2cTree := NewCpuTree(fmt.Sprintf("%s$%d", nodeTree.name, cache.ID()))
					l2cTree.level = CPUTopologyLevelL2Cache
					nodeTree.AddChild(l2cTree)

					threadsSeen := map[int]struct{}{}
					for _, cpuID := range cache.SharedCPUSet().List() {
						if _, alreadySeen := threadsSeen[cpuID]; alreadySeen {
							continue
						}
						cpu := sys.CPU(cpuID)
						coreTree := NewCpuTree(fmt.Sprintf("%scpu%d", nodeTree.name, cpuID))
						coreTree.level = CPUTopologyLevelCore
						l2cTree.AddChild(coreTree)
						for _, threadID := range cpu.ThreadCPUSet().List() {
							threadsSeen[threadID] = struct{}{}
							threadTree := NewCpuTree(fmt.Sprintf("%st%d", coreTree.name, threadID))
							threadTree.level = CPUTopologyLevelThread
							coreTree.AddChild(threadTree)
							threadTree.AddCpus(cpuset.New(threadID))
						}
					}
				}
			}
		}
	}
	return sysTree, nil
}

// ToAttributedSlice returns a CPU tree node and recursively all its
// child nodes in a slice that contains nodes with their attributes
// for allocation/releasing comparison.
// - currentCpus is the set of CPUs that can be freed in coming operation
// - freeCpus is the set of CPUs that can be allocated in coming operation
// - filter(tna) returns false if the node can be ignored
func (t *cpuTreeNode) ToAttributedSlice(
	currentCpus, freeCpus cpuset.CPUSet,
	filter func(*cpuTreeNodeAttributes) bool) []cpuTreeNodeAttributes {
	tnas := []cpuTreeNodeAttributes{}
	currentCpuCounts := []int{}
	freeCpuCounts := []int{}
	t.toAttributedSlice(currentCpus, freeCpus, filter, &tnas, 0, currentCpuCounts, freeCpuCounts)
	return tnas
}

func (t *cpuTreeNode) toAttributedSlice(
	currentCpus, freeCpus cpuset.CPUSet,
	filter func(*cpuTreeNodeAttributes) bool,
	tnas *[]cpuTreeNodeAttributes,
	depth int,
	currentCpuCounts []int,
	freeCpuCounts []int) {
	currentCpusHere := t.cpus.Intersection(currentCpus)
	freeCpusHere := t.cpus.Intersection(freeCpus)
	currentCpuCountHere := currentCpusHere.Size()
	currentCpuCountsHere := make([]int, len(currentCpuCounts)+1)
	copy(currentCpuCountsHere, currentCpuCounts)
	currentCpuCountsHere[depth] = currentCpuCountHere

	freeCpuCountHere := freeCpusHere.Size()
	freeCpuCountsHere := make([]int, len(freeCpuCounts)+1)
	copy(freeCpuCountsHere, freeCpuCounts)
	freeCpuCountsHere[depth] = freeCpuCountHere

	tna := cpuTreeNodeAttributes{
		t:                t,
		depth:            depth,
		currentCpus:      currentCpusHere,
		freeCpus:         freeCpusHere,
		currentCpuCount:  currentCpuCountHere,
		currentCpuCounts: currentCpuCountsHere,
		freeCpuCount:     freeCpuCountHere,
		freeCpuCounts:    freeCpuCountsHere,
	}

	if filter != nil && !filter(&tna) {
		return
	}

	*tnas = append(*tnas, tna)
	for _, child := range t.children {
		child.toAttributedSlice(currentCpus, freeCpus, filter,
			tnas, depth+1, currentCpuCountsHere, freeCpuCountsHere)
	}
}

// SplitLevel returns the root node of a new CPU tree where all
// branches of a topology level have been split into new classes.
func (t *cpuTreeNode) SplitLevel(splitLevel CPUTopologyLevel, cpuClassifier func(int) int) *cpuTreeNode {
	newRoot := t.CopyTree()
	if err := newRoot.DepthFirstWalk(func(tn *cpuTreeNode) error {
		// Dive into the level that will be split.
		if tn.level != splitLevel {
			return nil
		}
		// Classify CPUs to the map: class -> list of cpus
		classCpus := map[int][]int{}
		for _, cpu := range t.cpus.List() {
			class := cpuClassifier(cpu)
			classCpus[class] = append(classCpus[class], cpu)
		}
		// Clear existing children of this node. New children
		// will be classes whose children are masked versions
		// of original children of this node.
		origChildren := tn.children
		tn.children = make([]*cpuTreeNode, 0, len(classCpus))
		// Add new child corresponding each class.
		for class, cpus := range classCpus {
			cpuMask := cpuset.New(cpus...)
			newNode := NewCpuTree(fmt.Sprintf("%sclass%d", tn.name, class))
			tn.AddChild(newNode)
			newNode.cpus = tn.cpus.Intersection(cpuMask)
			newNode.level = tn.level
			newNode.parent = tn
			for _, child := range origChildren {
				newChild := child.CopyTree()
				if err := newChild.DepthFirstWalk(func(cn *cpuTreeNode) error {
					cn.cpus = cn.cpus.Intersection(cpuMask)
					if cn.cpus.Size() == 0 && cn.parent != nil {
						// all cpus masked
						// out: cut out this
						// branch
						newSiblings := []*cpuTreeNode{}
						for _, child := range cn.parent.children {
							if child != cn {
								newSiblings = append(newSiblings, child)
							}
						}
						cn.parent.children = newSiblings
						return WalkSkipChildren
					}
					return nil
				}); err != nil && err != WalkSkipChildren && err != WalkStop {
					log.Warnf("failed to walk CPU tree: %v", err)
				}
				newNode.AddChild(newChild)
			}
		}
		return WalkSkipChildren
	}); err != nil && err != WalkSkipChildren && err != WalkStop {
		log.Warnf("failed to walk CPU tree: %v", err)
	}
	return newRoot
}

// NewAllocator returns new CPU allocator for allocating CPUs from a
// CPU tree branch.
func (t *cpuTreeNode) NewAllocator(options cpuTreeAllocatorOptions) *cpuTreeAllocator {
	ta := &cpuTreeAllocator{
		root:    t,
		options: options,
	}
	if options.virtDevCpusets == nil {
		ta.cacheCloseCpuSets = map[string][]cpuset.CPUSet{}
	} else {
		ta.cacheCloseCpuSets = options.virtDevCpusets
	}
	if options.preferSpreadOnPhysicalCores {
		newTree := t.SplitLevel(CPUTopologyLevelNuma,
			// CPU classifier: class of the CPU equals to
			// the index in the child list of its parent
			// node in the tree. Expect leaf node is a
			// hyperthread, parent a physical core.
			func(cpu int) int {
				leaf := t.FindLeafWithCpu(cpu)
				if leaf == nil {
					log.Fatalf("SplitLevel CPU classifier: cpu %d not in tree:\n%s\n\n", cpu, t.PrettyPrint())
				}
				return leaf.SiblingIndex()
			})
		ta.root = newTree
	}
	return ta
}

// sorterAllocate implements an "is-less-than" callback that helps
// sorting a slice of cpuTreeNodeAttributes. The first item in the
// sorted list contains an optimal CPU tree node for allocating new
// CPUs.
func (ta *cpuTreeAllocator) sorterAllocate(tnas []cpuTreeNodeAttributes) func(int, int) bool {
	return func(i, j int) bool {
		if tnas[i].depth != tnas[j].depth {
			return tnas[i].depth > tnas[j].depth
		}
		for tdepth := 0; tdepth < len(tnas[i].currentCpuCounts); tdepth += 1 {
			// After this currentCpus will increase.
			// Maximize the maximal amount of currentCpus
			// as high level in the topology as possible.
			if tnas[i].currentCpuCounts[tdepth] != tnas[j].currentCpuCounts[tdepth] {
				return tnas[i].currentCpuCounts[tdepth] > tnas[j].currentCpuCounts[tdepth]
			}
		}
		for tdepth := 0; tdepth < len(tnas[i].freeCpuCounts); tdepth += 1 {
			// After this freeCpus will decrease.
			if tnas[i].freeCpuCounts[tdepth] != tnas[j].freeCpuCounts[tdepth] {
				if ta.options.topologyBalancing {
					// Goal: minimize maximal freeCpus in topology.
					return tnas[i].freeCpuCounts[tdepth] > tnas[j].freeCpuCounts[tdepth]
				} else {
					// Goal: maximize maximal freeCpus in topology.
					return tnas[i].freeCpuCounts[tdepth] < tnas[j].freeCpuCounts[tdepth]
				}
			}
		}
		return tnas[i].t.name < tnas[j].t.name
	}
}

// sorterRelease implements an "is-less-than" callback that helps
// sorting a slice of cpuTreeNodeAttributes. The first item in the
// list contains an optimal CPU tree node for releasing new CPUs.
func (ta *cpuTreeAllocator) sorterRelease(tnas []cpuTreeNodeAttributes) func(int, int) bool {
	return func(i, j int) bool {
		if tnas[i].depth != tnas[j].depth {
			return tnas[i].depth > tnas[j].depth
		}
		for tdepth := 0; tdepth < len(tnas[i].currentCpuCounts); tdepth += 1 {
			// After this currentCpus will decrease. Aim
			// to minimize the minimal amount of
			// currentCpus in order to decrease
			// fragmentation as high level in the topology
			// as possible.
			if tnas[i].currentCpuCounts[tdepth] != tnas[j].currentCpuCounts[tdepth] {
				return tnas[i].currentCpuCounts[tdepth] < tnas[j].currentCpuCounts[tdepth]
			}
		}
		for tdepth := 0; tdepth < len(tnas[i].freeCpuCounts); tdepth += 1 {
			// After this freeCpus will increase. Try to
			// maximize minimal free CPUs for better
			// isolation as high level in the topology as
			// possible.
			if tnas[i].freeCpuCounts[tdepth] != tnas[j].freeCpuCounts[tdepth] {
				if ta.options.topologyBalancing {
					return tnas[i].freeCpuCounts[tdepth] < tnas[j].freeCpuCounts[tdepth]
				} else {
					return tnas[i].freeCpuCounts[tdepth] < tnas[j].freeCpuCounts[tdepth]
				}
			}
		}
		return tnas[i].t.name > tnas[j].t.name
	}
}

// ResizeCpus implements topology awareness to both adding CPUs to and
// removing them from a set of CPUs. It returns CPUs from which actual
// allocation or releasing of CPUs can be done. ResizeCpus does not
// allocate or release CPUs.
//
// Parameters:
//   - currentCpus: a set of CPUs to/from which CPUs would be added/removed.
//   - freeCpus: a set of CPUs available CPUs.
//   - delta: number of CPUs to add (if positive) or remove (if negative).
//
// Return values:
//   - addFromCpus contains free CPUs from which delta CPUs can be
//     allocated. Note that the size of the set may be larger than
//     delta: there is room for other allocation logic to select from
//     these CPUs.
//   - removeFromCpus contains CPUs in currentCpus set from which
//     abs(delta) CPUs can be freed.
func (ta *cpuTreeAllocator) ResizeCpus(currentCpus, freeCpus cpuset.CPUSet, delta int) (cpuset.CPUSet, cpuset.CPUSet, error) {
	resizers := []cpuResizerFunc{
		ta.resizeCpusOnlyIfNecessary,
		ta.resizeCpusWithDynamicDeviceHints,
		ta.resizeCpusWithDevices,
		ta.resizeCpusOneAtATime,
		ta.resizeCpusMaxLocalSet,
		ta.resizeCpusNow}
	return ta.nextCpuResizer(resizers, currentCpus, freeCpus, delta)
}

type cpuResizerFunc func(resizers []cpuResizerFunc, currentCpus, freeCpus cpuset.CPUSet, delta int) (cpuset.CPUSet, cpuset.CPUSet, error)

func (ta *cpuTreeAllocator) nextCpuResizer(resizers []cpuResizerFunc, currentCpus, freeCpus cpuset.CPUSet, delta int) (cpuset.CPUSet, cpuset.CPUSet, error) {
	if len(resizers) == 0 {
		return freeCpus, currentCpus, fmt.Errorf("internal error: a CPU resizer consulted next resizer but there was no one left")
	}
	remainingResizers := resizers[1:]
	log.Debugf("- resizer-%d(%q, %q, %d)", len(remainingResizers), currentCpus, freeCpus, delta)
	addFrom, removeFrom, err := resizers[0](remainingResizers, currentCpus, freeCpus, delta)
	return addFrom, removeFrom, err
}

// resizeCpusNow does not call next resizer. Instead it keeps all CPU
// allocations from freeCpus and CPU releases from currentCpus equally
// good. This is the terminal block of resizers chain.
func (ta *cpuTreeAllocator) resizeCpusNow(resizers []cpuResizerFunc, currentCpus, freeCpus cpuset.CPUSet, delta int) (cpuset.CPUSet, cpuset.CPUSet, error) {
	return freeCpus, currentCpus, nil
}

// resizeCpusOnlyIfNecessary is the fast path for making trivial
// reservations and to fail if resizing is not possible.
func (ta *cpuTreeAllocator) resizeCpusOnlyIfNecessary(resizers []cpuResizerFunc, currentCpus, freeCpus cpuset.CPUSet, delta int) (cpuset.CPUSet, cpuset.CPUSet, error) {
	switch {
	case delta == 0:
		// Nothing to do.
		return emptyCpuSet, emptyCpuSet, nil
	case delta > 0:
		if freeCpus.Size() < delta {
			return freeCpus, emptyCpuSet, fmt.Errorf("not enough free CPUs (%d) to resize current CPU set from %d to %d CPUs", freeCpus.Size(), currentCpus.Size(), currentCpus.Size()+delta)
		} else if freeCpus.Size() == delta {
			// Allocate all the remaining free CPUs.
			return freeCpus, emptyCpuSet, nil
		}
	case delta < 0:
		if currentCpus.Size() < -delta {
			return emptyCpuSet, currentCpus, fmt.Errorf("not enough current CPUs (%d) to release %d CPUs", currentCpus.Size(), -delta)
		} else if currentCpus.Size() == -delta {
			// Free all allocated CPUs.
			return emptyCpuSet, currentCpus, nil
		}
	}
	return ta.nextCpuResizer(resizers, currentCpus, freeCpus, delta)
}

// resizeCpusWithDynamicDeviceHints handles allocating CPUs in
// scenarios where each selected CPU may change the set of CPUs are
// good to be selected next.
func (ta *cpuTreeAllocator) resizeCpusWithDynamicDeviceHints(resizers []cpuResizerFunc, currentCpus, freeCpus cpuset.CPUSet, delta int) (cpuset.CPUSet, cpuset.CPUSet, error) {
	// If the deviceUpdateOnEveryCpu callback is set, call it
	// after each CPU allocation to update the state of virtual
	// devices. If not set or if CPUs are released instead of
	// allocated, do nothing but forward the call to next
	// resizers.
	if ta.options.deviceUpdateOnEveryCpu == nil {
		return ta.nextCpuResizer(resizers, currentCpus, freeCpus, delta)
	}
	ta.options.deviceUpdateOnEveryCpu(currentCpus)
	if delta <= 0 {
		return ta.nextCpuResizer(resizers, currentCpus, freeCpus, delta)
	}
	// Update virtual devices on every CPU allocation. Request
	// first allocation of all delta CPUs, but choose only one CPU
	// from returned CPU set. Requesting initially a large set of
	// CPUs increases likelihood that the first CPU that we choose
	// into the addedCpus works as a good seed for getting many
	// CPUs that are close to each other.
	addFrom, removeFrom, err := ta.nextCpuResizer(resizers, currentCpus, freeCpus, delta)
	if err != nil || addFrom.Size() < delta {
		return addFrom, removeFrom, err
	}
	addedCpus := cpuset.New()
	for {
		addedCpu := addFrom.List()[0]
		addedCpus = addedCpus.Union(cpuset.New(addedCpu))
		if addedCpus.Size() >= delta {
			break
		}
		currentCpus = currentCpus.Union(cpuset.New(addedCpu))
		freeCpus = freeCpus.Difference(currentCpus)
		ta.options.deviceUpdateOnEveryCpu(currentCpus)
		addFrom, removeFrom, err = ta.nextCpuResizer(resizers, currentCpus, freeCpus, 1)
		if err != nil || addFrom.Size() < 1 {
			return addedCpus, removeFrom, err
		}
	}
	return addedCpus.Union(addFrom), removeFrom, err
}

// resizeCpusWithDevices prefers allocating CPUs from those freeCpus
// that are topologically close to preferred devices, and releasing
// those currentCpus that are not.
func (ta *cpuTreeAllocator) resizeCpusWithDevices(resizers []cpuResizerFunc, currentCpus, freeCpus cpuset.CPUSet, delta int) (cpuset.CPUSet, cpuset.CPUSet, error) {
	// allCloseCpuSets contains cpusets in the order of priority.
	// Applying the first cpusets in it are prioritized over ones
	// after them.
	allCloseCpuSets := [][]cpuset.CPUSet{}
	for _, devPath := range ta.options.preferCloseToDevices {
		if closeCpuSets := ta.topologyHintCpus(devPath); len(closeCpuSets) > 0 {
			allCloseCpuSets = append(allCloseCpuSets, closeCpuSets)
		}
	}
	for _, devPath := range ta.options.preferFarFromDevices {
		for _, farCpuSet := range ta.topologyHintCpus(devPath) {
			allCloseCpuSets = append(allCloseCpuSets, []cpuset.CPUSet{freeCpus.Difference(farCpuSet)})
		}
	}
	if len(allCloseCpuSets) == 0 {
		return ta.nextCpuResizer(resizers, currentCpus, freeCpus, delta)
	}
	if delta > 0 {
		// Allocate N=delta CPUs from freeCpus based on topology hints.
		// Build a new set of freeCpus with at least N CPUs based on
		// intersection with CPU hints.
		// In case of conflicting topology hints the first
		// hints in the list are the most important.
		remainingFreeCpus := freeCpus
		appliedHints := 0
		totalHints := 0
		for _, closeCpuSets := range allCloseCpuSets {
			for _, cpus := range closeCpuSets {
				totalHints++
				newRemainingFreeCpus := remainingFreeCpus.Intersection(cpus)
				if newRemainingFreeCpus.Size() >= delta {
					appliedHints++
					log.Debugf("  - take hinted cpus %q, common free %q", cpus, newRemainingFreeCpus)
					remainingFreeCpus = newRemainingFreeCpus
				} else {
					log.Debugf("  - drop hinted cpus %q, not enough common free in %q", cpus, newRemainingFreeCpus)
				}
			}
		}
		log.Debugf("  - original free cpus %q, took %d/%d hints, remaining free: %q",
			freeCpus, appliedHints, totalHints, remainingFreeCpus)
		return ta.nextCpuResizer(resizers, currentCpus, remainingFreeCpus, delta)
	} else if delta < 0 {
		// Free N=-delta CPUs from currentCpus based on topology hints.
		// 1. Sort currentCpus based on topology hints (leastHintedCpus).
		// 2. Pick largest hint value that has to be released (maxHints).
		// 3. Free all CPUs that have a hint value smaller than maxHints.
		// 4. Let next CPU resizer choose CPUs to be freed among
		//    CPUs with hint value maxHints.
		currentCpuHints := map[int]uint64{}
		for hintPriority, closeCpuSets := range allCloseCpuSets {
			for _, cpus := range closeCpuSets {
				for _, cpu := range cpus.Intersection(currentCpus).UnsortedList() {
					currentCpuHints[cpu] += 1 << (len(allCloseCpuSets) - 1 - hintPriority)
				}
			}
		}
		leastHintedCpus := currentCpus.UnsortedList()
		sort.Slice(leastHintedCpus, func(i, j int) bool {
			return currentCpuHints[leastHintedCpus[i]] < currentCpuHints[leastHintedCpus[j]]
		})
		maxHints := currentCpuHints[leastHintedCpus[-delta]]
		currentToFreeForSure := cpuset.New()
		currentToFreeMaybe := cpuset.New()
		for i := 0; i < len(leastHintedCpus) && currentCpuHints[leastHintedCpus[i]] <= maxHints; i++ {
			if currentCpuHints[leastHintedCpus[i]] < maxHints {
				currentToFreeForSure = currentToFreeForSure.Union(cpuset.New(leastHintedCpus[i]))
			} else {
				currentToFreeMaybe = currentToFreeMaybe.Union(cpuset.New(leastHintedCpus[i]))
			}
		}
		remainingDelta := delta + currentToFreeForSure.Size()
		log.Debugf("  - device hints: from cpus %q: free for sure: %q and %d more from: %q",
			currentCpus, currentToFreeForSure, -remainingDelta, currentToFreeMaybe)
		_, freeFromMaybe, err := ta.nextCpuResizer(resizers, currentToFreeMaybe, freeCpus, remainingDelta)
		// Do not include possible extra CPUs from
		// freeFromMaybe to make sure that all CPUs with least
		// hints will be freed.
		for _, cpu := range freeFromMaybe.UnsortedList() {
			if currentToFreeForSure.Size() >= -delta {
				break
			}
			currentToFreeForSure = currentToFreeForSure.Union(cpuset.New(cpu))
		}
		return freeCpus, currentToFreeForSure, err
	}
	return freeCpus, currentCpus, nil
}

// Fetch cached topology hint, return error only once per bad dev
func (ta *cpuTreeAllocator) topologyHintCpus(dev string) []cpuset.CPUSet {
	if closeCpuSets, ok := ta.cacheCloseCpuSets[dev]; ok {
		return closeCpuSets
	}
	topologyHints, err := topology.NewTopologyHints(dev)
	if err != nil {
		log.Errorf("failed to find topology of device %q: %v", dev, err)
		ta.cacheCloseCpuSets[dev] = []cpuset.CPUSet{}
	} else {
		for _, topologyHint := range topologyHints {
			ta.cacheCloseCpuSets[dev] = append(ta.cacheCloseCpuSets[dev], cpuset.MustParse(topologyHint.CPUs))
		}
	}
	return ta.cacheCloseCpuSets[dev]
}

func (ta *cpuTreeAllocator) resizeCpusOneAtATime(resizers []cpuResizerFunc, currentCpus, freeCpus cpuset.CPUSet, delta int) (cpuset.CPUSet, cpuset.CPUSet, error) {
	if delta > 0 {
		addFromSuperset, removeFromSuperset, err := ta.nextCpuResizer(resizers, currentCpus, freeCpus, delta)
		if !ta.options.preferSpreadOnPhysicalCores || addFromSuperset.Size() == delta {
			return addFromSuperset, removeFromSuperset, err
		}
		// addFromSuperset contains more CPUs (equally good
		// choices) than actually needed. In case of
		// preferSpreadOnPhysicalCores, however, selecting any
		// of these does not result in equally good
		// result. Therefore, in this case, construct addFrom
		// set by adding one CPU at a time.
		addFrom := cpuset.New()
		for n := 0; n < delta; n++ {
			addSingleFrom, _, err := ta.nextCpuResizer(resizers, currentCpus, freeCpus, 1)
			if err != nil {
				return addFromSuperset, removeFromSuperset, err
			}
			if addSingleFrom.Size() != 1 {
				return addFromSuperset, removeFromSuperset, fmt.Errorf("internal error: failed to find single CPU to allocate, "+
					"currentCpus=%s freeCpus=%s expectedSingle=%s",
					currentCpus, freeCpus, addSingleFrom)
			}
			addFrom = addFrom.Union(addSingleFrom)
			if addFrom.Size() != n+1 {
				return addFromSuperset, removeFromSuperset, fmt.Errorf("internal error: double add the same CPU (%s) to cpuset %s on round %d",
					addSingleFrom, addFrom, n+1)
			}
			currentCpus = currentCpus.Union(addSingleFrom)
			freeCpus = freeCpus.Difference(addSingleFrom)
		}
		return addFrom, removeFromSuperset, nil
	}
	// In multi-CPU removal, remove CPUs one by one instead of
	// trying to find a single topology element from which all of
	// them could be removed.
	removeFrom := cpuset.New()
	addFrom := cpuset.New()
	for n := 0; n < -delta; n++ {
		_, removeSingleFrom, err := ta.nextCpuResizer(resizers, currentCpus, freeCpus, -1)
		if err != nil {
			return addFrom, removeFrom, err
		}
		// Make cheap internal error checks in order to capture
		// issues in alternative algorithms.
		if removeSingleFrom.Size() != 1 {
			return addFrom, removeFrom, fmt.Errorf("internal error: failed to find single cpu to free, "+
				"currentCpus=%s freeCpus=%s expectedSingle=%s",
				currentCpus, freeCpus, removeSingleFrom)
		}
		if removeFrom.Union(removeSingleFrom).Size() != n+1 {
			return addFrom, removeFrom, fmt.Errorf("internal error: double release of a cpu, "+
				"currentCpus=%s freeCpus=%s alreadyRemoved=%s removedNow=%s",
				currentCpus, freeCpus, removeFrom, removeSingleFrom)
		}
		removeFrom = removeFrom.Union(removeSingleFrom)
		currentCpus = currentCpus.Difference(removeSingleFrom)
		freeCpus = freeCpus.Union(removeSingleFrom)
	}
	return addFrom, removeFrom, nil
}

func (ta *cpuTreeAllocator) resizeCpusMaxLocalSet(resizers []cpuResizerFunc, currentCpus, freeCpus cpuset.CPUSet, delta int) (cpuset.CPUSet, cpuset.CPUSet, error) {
	tnas := ta.root.ToAttributedSlice(currentCpus, freeCpus,
		func(tna *cpuTreeNodeAttributes) bool {
			// filter out branches with insufficient cpus
			if delta > 0 && tna.freeCpuCount-delta < 0 {
				// cannot allocate delta cpus
				return false
			}
			if delta < 0 && tna.currentCpuCount+delta < 0 {
				// cannot release delta cpus
				return false
			}
			return true
		})

	// Sort based on attributes
	if delta > 0 {
		sort.Slice(tnas, ta.sorterAllocate(tnas))
	} else {
		sort.Slice(tnas, ta.sorterRelease(tnas))
	}
	if len(tnas) == 0 {
		return freeCpus, currentCpus, fmt.Errorf("not enough free CPUs")
	}
	return ta.nextCpuResizer(resizers, tnas[0].currentCpus, tnas[0].freeCpus, delta)
}
