// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/elastic/elastic-agent/internal/pkg/core/state"
	"github.com/elastic/elastic-agent/internal/pkg/sorted"
)

const (
	configuredType = "configured"
	internalType   = "internal"
)

type sourceInfo struct {
	// Kind is a kind of process e.g configured or internal
	// configured - used for user configured processes
	// internal - used for monitoring processes
	Kind string `json:"kind"`

	// Outputs process is handling.
	Outputs []string `json:"outputs"`
}

type processInfo struct {
	// ID is a unique id of the process.
	ID string `json:"id"`

	// PID is a current process ID.
	PID string `json:"pid"`

	// Binary name e.g filebeat, this does not contain absolute path.
	Binary string `json:"binary"`

	// Source information
	Source sourceInfo `json:"source"`
}

type processesResponse struct {
	Processes []processInfo `json:"processes"`
}

type errResponse struct {
	// Type is a type of error
	Type string `json:"type"`

	// Reason is a detailed error message
	Reason string `json:"reason"`
}

type stater interface {
	State() map[string]state.State
}

func processesHandler(routesFetchFn func() *sorted.Set) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		resp := processesResponse{
			Processes: processesFromRoutes(routesFetchFn),
		}

		writeResponse(w, resp)
	}
}

func processesFromRoutes(routesFetchFn func() *sorted.Set) []processInfo {
	var processes []processInfo
	routes := routesFetchFn()

	for _, k := range routes.Keys() {
		op, found := routes.Get(k)
		if !found {
			continue
		}

		s, ok := op.(stater)
		if !ok {
			continue
		}

		states := s.State()

		for app, state := range states {
			binaryName, isMonitoring := appNameFromDescriptor(app)
			appType := configuredType
			if isMonitoring {
				appType = internalType
			}

			var pid int
			if state.ProcessInfo != nil {
				pid = state.ProcessInfo.PID
			}

			processInfo := processInfo{
				ID:     processID(k, binaryName, isMonitoring),
				PID:    strconv.Itoa(pid),
				Binary: binaryName,
				Source: sourceInfo{
					Kind:    appType,
					Outputs: []string{k},
				},
			}

			processes = append(processes, processInfo)
		}
	}

	return processes
}

func processID(output, binaryName string, isMonitoring bool) string {
	id := binaryName + separator + output
	if isMonitoring {
		return id + monitoringSuffix
	}

	return id
}

func appNameFromDescriptor(d string) (string, bool) {
	// monitoring desctiptor contains suffix with tag
	// non monitoring just `binaryname--version`
	parts := strings.Split(d, "--")
	return parts[0], len(parts) > 2
}