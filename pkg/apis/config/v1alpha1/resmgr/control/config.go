// Copyright The NRI Plugins Authors. All Rights Reserved.
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

package control

import (
	"github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/control/blockio"
	"github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/control/cpu"
	"github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/control/rdt"
)

// +kubebuilder:object:generate=true
type Config struct {
	// +optional
	CPU cpu.Config `json:"cpu,omitempty"`
	// +optional
	RDT rdt.Config `json:"rdt,omitempty"`
	// +optional
	BlockIO blockio.Config `json:"blockio,omitempty"`
}
