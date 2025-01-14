//go:build linux
// +build linux

// Copyright 2020 Google Inc. All Rights Reserved.
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

// Manager of resctrl for containers.
package resctrl

import (
	"os"

	"github.com/google/cadvisor/stats"

	"github.com/opencontainers/runc/libcontainer/intelrdt"
)

type manager struct {
	id string
	stats.NoopDestroy
}

func (m manager) GetCollector(resctrlPath string) (stats.Collector, error) {
	if _, err := os.Stat(resctrlPath); err != nil {
		return &stats.NoopCollector{}, err
	}
	collector := newCollector(m.id, resctrlPath)
	return collector, nil
}

func NewManager(id string) (stats.Manager, error) {

	if intelrdt.IsMBMEnabled() || intelrdt.IsCMTEnabled() {
		return &manager{id: id}, nil
	}

	return &stats.NoopManager{}, nil
}
