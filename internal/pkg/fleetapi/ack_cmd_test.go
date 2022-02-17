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

package fleetapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elastic/elastic-agent/internal/pkg/fleetapi/client"
)

func TestAck(t *testing.T) {
	const withAPIKey = "secret"
	agentInfo := &agentinfo{}

	t.Run("Test ack roundtrip", withServerWithAuthClient(
		func(t *testing.T) *http.ServeMux {
			raw := `{"action": "ack"}`
			mux := http.NewServeMux()
			path := fmt.Sprintf("/api/fleet/agents/%s/acks", agentInfo.AgentID())
			mux.HandleFunc(path, authHandler(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)

				responses := struct {
					Events []AckEvent `json:"events"`
				}{}

				decoder := json.NewDecoder(r.Body)
				defer r.Body.Close()

				err := decoder.Decode(&responses)
				require.NoError(t, err)

				require.Equal(t, 1, len(responses.Events))

				id := responses.Events[0].ActionID
				require.Equal(t, "my-id", id)

				fmt.Fprint(w, raw)
			}, withAPIKey))
			return mux
		}, withAPIKey,
		func(t *testing.T, client client.Sender) {
			action := &ActionPolicyChange{
				ActionID:   "my-id",
				ActionType: "POLICY_CHANGE",
				Policy: map[string]interface{}{
					"id": "config_id",
				},
			}

			cmd := NewAckCmd(&agentinfo{}, client)

			request := AckRequest{
				Events: []AckEvent{
					{
						EventType: "ACTION_RESULT",
						SubType:   "ACKNOWLEDGED",
						ActionID:  action.ID(),
					},
				},
			}

			r, err := cmd.Execute(context.Background(), &request)
			require.NoError(t, err)
			require.Equal(t, "ack", r.Action)
		},
	))
}