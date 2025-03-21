// Copyright 2019 Intel Corporation. All Rights Reserved.
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
	"errors"
	"fmt"
	"sort"
	"strings"

	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/containers/nri-plugins/pkg/resmgr/cache"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/control"
)

// Control is the interface for triggering controller-/domain-specific post-decision actions.
type Control interface {
	// StartStopControllers starts/stops all controllers according to configuration.
	StartStopControllers(*cfgapi.Config) error
	// PreCreateHooks runs the pre-create hooks of all registered controllers.
	RunPreCreateHooks(cache.Container) error
	// RunPreStartHooks runs the pre-start hooks of all registered controllers.
	RunPreStartHooks(cache.Container) error
	// RunPostStartHooks runs the post-start hooks of all registered controllers.
	RunPostStartHooks(cache.Container) error
	// RunPostUpdateHooks runs the post-update hooks of all registered controllers.
	RunPostUpdateHooks(cache.Container) error
	// RunPostStopHooks runs the post-stop hooks of all registered controllers.
	RunPostStopHooks(cache.Container) error
}

// Controller is the interface all resource controllers must implement.
type Controller interface {
	// Start prepares the controller for resource control/decision enforcement.
	Start(cache.Cache, *cfgapi.Config) (bool, error)
	// Stop shuts down the controller.
	Stop()
	// PreCreateHook is the controller's pre-create hook.
	PreCreateHook(cache.Container) error
	// PreStartHook is the controller's pre-start hook.
	PreStartHook(cache.Container) error
	// PostStartHook is the controller's post-start hook.
	PostStartHook(cache.Container) error
	// PostUpdateHook is the controller's post-update hook.
	PostUpdateHook(cache.Container) error
	// PostStopHook is the controller's post-stop hook.
	PostStopHook(cache.Container) error
}

// control encapsulates our controller-agnostic runtime state.
type control struct {
	cache       cache.Cache    // resource manager cache
	controllers []*controller  // active controllers
	cfg         *cfgapi.Config // runtime configuration
}

// controller represents a single registered controller.
type controller struct {
	name        string     // controller name
	description string     // controller description
	c           Controller // controller interface
	running     bool       // whether the controller is running
}

// our hook names
const (
	precreate  = "pre-create"
	prestart   = "pre-start"
	poststart  = "post-start"
	postupdate = "post-update"
	poststop   = "post-stop"
)

// All registered controllers.
var controllers = make(map[string]*controller)

// Our logger instance.
var log logger.Logger = logger.NewLogger("resource-control")

// NewControl creates a new controller-agnostic instance.
func NewControl(cc cache.Cache) (Control, error) {
	c := &control{
		cache: cc,
	}

	for _, controller := range controllers {
		c.controllers = append(c.controllers, controller)
	}
	sort.Slice(c.controllers,
		func(i, j int) bool {
			return strings.Compare(c.controllers[i].name, c.controllers[j].name) < 0
		})

	return c, nil
}

// StartStopController starts/stops all controllers according to configuration.
func (c *control) StartStopControllers(cfg *cfgapi.Config) error {
	var errs []error

	c.cfg = cfg.DeepCopy()

	log.Info("syncing controllers with configuration...")

	for _, controller := range c.controllers {
		if controller.running {
			log.Infof("stopping controller %s", controller.name)
			controller.c.Stop()
			controller.running = false
		}
	}

	for _, controller := range c.controllers {
		log.Infof("starting controller %s", controller.name)
		enabled, err := controller.c.Start(c.cache, cfg.DeepCopy())
		if err != nil {
			errs = append(errs, controlError("%s failed to start: %v", controller.name, err))
		} else {
			if enabled {
				log.Infof("controller %s is enabled and running", controller.name)
				controller.running = true
			} else {
				log.Infof("controller %s is disabled", controller.name)
			}
		}
	}

	return errors.Join(errs...)
}

// RunPreCreateHooks runs all registered controllers' PreCreate hooks.
func (c *control) RunPreCreateHooks(container cache.Container) error {
	for _, controller := range c.controllers {
		if err := c.runhook(controller, precreate, container); err != nil {
			return err
		}
	}
	return nil
}

// RunPreStartHooks runs all registered controllers' PreStart hooks.
func (c *control) RunPreStartHooks(container cache.Container) error {
	for _, controller := range c.controllers {
		if err := c.runhook(controller, prestart, container); err != nil {
			return err
		}
	}
	return nil
}

// RunPostStartHooks runs all registered controllers' PostStart hooks.
func (c *control) RunPostStartHooks(container cache.Container) error {
	for _, controller := range c.controllers {
		if err := c.runhook(controller, poststart, container); err != nil {
			return err
		}
	}
	return nil
}

// RunPostUpdateHooks runs all registered controllers' PostUpdate hooks.
func (c *control) RunPostUpdateHooks(container cache.Container) error {
	for _, controller := range c.controllers {
		if err := c.runhook(controller, postupdate, container); err != nil {
			return err
		}
	}
	return nil
}

// RunPostStopHooks runs all registered controllers' PostStop hooks.
func (c *control) RunPostStopHooks(container cache.Container) error {
	for _, controller := range c.controllers {
		if err := c.runhook(controller, poststop, container); err != nil {
			return err
		}
	}
	return nil
}

// runhook executes the given container hook according to the controller settings
func (c *control) runhook(controller *controller, hook string, container cache.Container) error {
	if !controller.running {
		return nil
	}

	var fn func(cache.Container) error

	switch hook {
	case precreate:
		fn = controller.c.PreCreateHook
	case prestart:
		fn = controller.c.PreStartHook
	case poststart:
		fn = controller.c.PostStartHook
	case postupdate:
		fn = controller.c.PostUpdateHook
	case poststop:
		fn = controller.c.PostStopHook
	}

	log.Debug("running %s %s hook for container %s", controller.name, hook, container.PrettyName())

	if err := fn(container); err != nil {
		return controlError("%s %s hook failed: %v", controller.name, hook, err)
	}

	return nil
}

// Register registers a new controller.
func Register(name, description string, c Controller) error {
	log.Info("registering controller %s...", name)

	if oc, ok := controllers[name]; ok {
		return controlError("controller %s (%s) already registered.", oc.name, oc.description)
	}

	controllers[name] = &controller{
		name:        name,
		description: description,
		c:           c,
	}

	return nil
}

// controlError returns a controller-specific formatted error.
func controlError(format string, args ...interface{}) error {
	return fmt.Errorf("control: "+format, args...)
}
