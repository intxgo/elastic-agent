// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

//go:build integration

package ess

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"

	integrationtest "github.com/elastic/elastic-agent/pkg/testing"
	"github.com/elastic/elastic-agent/pkg/testing/define"
	"github.com/elastic/elastic-agent/pkg/testing/tools/check"
	"github.com/elastic/elastic-agent/pkg/testing/tools/testcontext"
	"github.com/elastic/elastic-agent/testing/fleetservertest"
	"github.com/elastic/elastic-agent/testing/integration"
)

func TestInspect(t *testing.T) {
	_ = define.Require(t, define.Requirements{
		Group: integration.Fleet,
		Local: false,
		Sudo:  true,
	})

	ctx, cancel := testcontext.WithTimeout(t, context.Background(), time.Minute*10)
	defer cancel()

	apiKey, policy := createBasicFleetPolicyData(t, "http://fleet-server:8220")
	checkinWithAcker := fleetservertest.NewCheckinActionsWithAcker()
	fleet := fleetservertest.NewServerWithHandlers(
		apiKey,
		"enrollmentToken",
		policy.AgentID,
		policy.PolicyID,
		checkinWithAcker.ActionsGenerator(),
		checkinWithAcker.Acker(),
		fleetservertest.WithRequestLog(t.Logf),
	)
	defer fleet.Close()
	policyChangeAction, err := fleetservertest.NewActionPolicyChangeWithFakeComponent("test-policy-change", fleetservertest.TmplPolicy{
		AgentID:    policy.AgentID,
		PolicyID:   policy.PolicyID,
		FleetHosts: []string{fleet.LocalhostURL},
	})
	require.NoError(t, err)
	checkinWithAcker.AddCheckin("token", 0, policyChangeAction)

	fixture, err := define.NewFixtureFromLocalBuild(t,
		define.Version(),
		integrationtest.WithAllowErrors(),
		integrationtest.WithLogOutput())
	require.NoError(t, err, "SetupTest: NewFixtureFromLocalBuild failed")
	err = fixture.EnsurePrepared(ctx)
	require.NoError(t, err, "SetupTest: fixture.Prepare failed")

	out, err := fixture.Install(
		ctx,
		&integrationtest.InstallOpts{
			Force:          true,
			NonInteractive: true,
			Insecure:       true,
			Privileged:     false,
			EnrollOpts: integrationtest.EnrollOpts{
				URL:             fleet.LocalhostURL,
				EnrollmentToken: "anythingWillDO",
			}})
	require.NoErrorf(t, err, "Error when installing agent, output: %s", out)
	check.ConnectedToFleet(ctx, t, fixture, 5*time.Minute)
	require.Eventually(t, func() bool {
		return checkinWithAcker.Acked(policyChangeAction.ActionID)
	}, 5*time.Minute, time.Second, "Policy change action should have been acked")

	p, err := fixture.Exec(ctx, []string{"inspect"})
	require.NoErrorf(t, err, "Error when running inspect, output: %s", p)
	// Unmarshal into minimal object just to check if a secret has been redacted.
	var yObj struct {
		Agent struct {
			Protection struct {
				SigningKey         string `yaml:"signing_key"`
				UninstallTokenHash string `yaml:"uninstall_token_hash"`
			} `yaml:"protection"`
		} `yaml:"agent"`
		SecretPaths []string `yaml:"secret_paths"`
		Inputs      []struct {
			CustomAttr string `yaml:"custom_attr"`
		} `yaml:"inputs"`
	}
	err = yaml.Unmarshal(p, &yObj)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"inputs.0.custom_attr"}, yObj.SecretPaths)
	require.Len(t, yObj.Inputs, 1)
	assert.Equalf(t, "<REDACTED>", yObj.Inputs[0].CustomAttr, "inspect output: %s", p)
	assert.Equalf(t, "<REDACTED>", yObj.Agent.Protection.SigningKey, "`signing_key` is not redacted but it should be, because it contains `key`. inspect output: %s", p)
	assert.Equalf(t, "<REDACTED>", yObj.Agent.Protection.UninstallTokenHash, "`uninstall_token_hash` is not redacted but it should be, because it contains `token`. inspect output: %s", p)
}
