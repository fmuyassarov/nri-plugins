# Make sure all the pods in default namespace are cleared so we get a fresh start
vm-command "kubectl delete pods --all --now"

# Remove also any leftover test pods from kube-system
vm-command "kubectl delete pods pod0 pod1 pod2 pod3 pod4 pod5 --ignore-not-found=true --now -n kube-system"

# Cleanup kernel commandline, otherwise isolcpus will affect CPU
# pinning and cause false negatives from other tests on this VM.
# This can happen if test08-isolcpus failed and we are re-running
# the tests from the start.
vm-command "grep isolcpus /proc/cmdline" && {
    vm-set-kernel-cmdline ""
    timeout=120 vm-reboot
    vm-command "grep isolcpus /proc/cmdline" && {
	error "failed to clean up isolcpus kernel commandline parameter"
    }
    echo "isolcpus removed from kernel commandline"
    vm-command "systemctl restart kubelet"
    vm-wait-process --timeout 120 kube-apiserver
}

# Do a fresh start
helm-terminate
helm_config=$(instantiate helm-config.yaml) helm-launch topology-aware

# pod0: Test that 4 guaranteed containers eligible for isolated CPU allocation
# gets evenly spread over NUMA nodes.
CONTCOUNT=4 CPU=1 create guaranteed
report allowed
verify \
    'len(cpus["pod0c0"]) == 1' \
    'len(cpus["pod0c1"]) == 1' \
    'len(cpus["pod0c2"]) == 1' \
    'len(cpus["pod0c3"]) == 1' \
    'disjoint_sets(cpus["pod0c0"], cpus["pod0c1"], cpus["pod0c2"], cpus["pod0c3"])' \
    'disjoint_sets(nodes["pod0c0"], nodes["pod0c1"], nodes["pod0c2"], nodes["pod0c3"])'

vm-command "kubectl delete pods --all --now"

# pod1: Test that 4 guaranteed containers not eligible for isolated CPU allocation
# gets evenly spread over NUMA nodes.
CONTCOUNT=4 CPU=3 ANN0='hide-hyperthreads.resource-policy.nri.io/container.pod1c2: "true"' create guaranteed
report allowed
verify \
    'len(cpus["pod1c0"]) == 3' \
    'len(cpus["pod1c1"]) == 3' \
    'len(cpus["pod1c2"]) == 2' \
    'len(cores["pod1c2"]) == 2' \
    'len(cpus["pod1c3"]) == 3' \
    'disjoint_sets(cpus["pod1c0"], cpus["pod1c1"], cpus["pod1c2"], cpus["pod1c3"])' \
    'disjoint_sets(nodes["pod1c0"], nodes["pod1c1"], nodes["pod1c2"], nodes["pod1c3"])'

vm-command "kubectl delete pods --all --now"

# pod2: Test that 4 burstable containers not eligible for isolated/exclusive CPU allocation
# gets evenly spread over NUMA nodes.
CONTCOUNT=4 CPUREQ=2 CPULIM=4 create burstable
report allowed
verify \
    'disjoint_sets(cpus["pod2c0"], cpus["pod2c1"], cpus["pod2c2"], cpus["pod2c3"])' \
    'disjoint_sets(nodes["pod2c0"], nodes["pod2c1"], nodes["pod2c2"], nodes["pod2c3"])'

vm-command "kubectl delete pods --all --now"

# pod3: Test that initContainer resources are freed before launching
# containers: instantiate 5 init containers, each requiring 5 CPUs. If
# the resources of an init container weren't freed before next init
# container is launched, not all of them could be launched, and not
# real containers could fit on the node.
ICONTCOUNT=5 ICONTSLEEP=1 CONTCOUNT=2 CPU=5 MEM=100M create guaranteed
report allowed
verify \
    'disjoint_sets(cpus["pod3c0"], cpus["pod3c1"])' \
    'disjoint_sets(nodes["pod3c0"], nodes["pod3c1"])' \
    'disjoint_sets(packages["pod3c0"], packages["pod3c1"])'

vm-command "kubectl delete pods --all --now"

# pod4: Test that with pod colocation enabled containers within a pod get
# colocated (assigned topologically close to each other) as opposed to being
# evenly spread out.
helm-terminate
helm_config=$(COLOCATE_PODS=true instantiate helm-config.yaml) helm-launch topology-aware

CONTCOUNT=4 CPU=100m create guaranteed
report allowed
verify \
    'cpus["pod4c1"] == cpus["pod4c0"]' \
    'cpus["pod4c2"] == cpus["pod4c0"]' \
    'cpus["pod4c3"] == cpus["pod4c0"]'

vm-command "kubectl delete pods --all --now"

# pod{5,6,7}: Test that with namespace colocation enabled containers of pods
# in the same namespace get colocated (assigned topologically close to each
# other) as opposed to being evenly spread out.
helm-terminate
helm_config=$(COLOCATE_NAMESPACES=true instantiate helm-config.yaml) helm-launch topology-aware

vm-command "kubectl create namespace test-ns"

CONTCOUNT=1 CPU=100m namespace=test-ns create guaranteed
CONTCOUNT=1 CPU=100m namespace=test-ns create guaranteed
CONTCOUNT=2 CPU=100m namespace=test-ns create guaranteed
report allowed
verify \
    'cpus["pod6c0"] == cpus["pod5c0"]' \
    'cpus["pod7c0"] == cpus["pod5c0"]' \
    'cpus["pod7c1"] == cpus["pod5c0"]'

vm-command "kubectl delete pods -n test-ns --all --now"
vm-command "kubectl delete namespace test-ns"

# Restore default test configuration, restart nri-resource-policy.
helm-terminate

echo $?
