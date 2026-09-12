package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type recordedCommand struct {
	name string
	args []string
}

type fakeRunner struct {
	calls       []recordedCommand
	runFailures map[string]error
	output      []byte
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) error {
	f.calls = append(f.calls, recordedCommand{name: name, args: append([]string(nil), args...)})
	return f.runFailures[strings.Join(append([]string{name}, args...), " ")]
}

func (f *fakeRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, recordedCommand{name: name, args: append([]string(nil), args...)})
	return f.output, nil
}

func TestParseConfig(t *testing.T) {
	config, err := parseConfig([]byte(`{
        "version":"2",
        "gatewayIPs":[{"interface":"subnet-a","address":"10.220.1.1/24"}],
        "routes":[{"destination":"10.240.0.0/16","interface":"subnet-a"}]
    }`))
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}
	if config.Version != "2" || len(config.GatewayIP) != 1 || len(config.Routes) != 1 {
		t.Fatalf("parseConfig() = %#v", config)
	}
}

func TestParseConfigRejectsIncompleteRoute(t *testing.T) {
	_, err := parseConfig([]byte(`{"gatewayIPs":[],"routes":[{"destination":"10.0.0.0/8"}]}`))
	if err == nil || !strings.Contains(err.Error(), "routes[0].interface is required") {
		t.Fatalf("parseConfig() error = %v", err)
	}
}

func TestReconcileAppliesDesiredAddressesAndRoutes(t *testing.T) {
	runner := &fakeRunner{runFailures: map[string]error{}}
	desired := RouterConfig{
		GatewayIP: []GatewayIPSpec{{Interface: "subnet-a", Address: "10.220.1.1/24"}},
		Routes:    []RouteSpec{{Destination: "10.240.0.0/16", Interface: "subnet-a"}},
	}
	if err := reconcile(context.Background(), runner, RouterConfig{}, desired); err != nil {
		t.Fatalf("reconcile() error = %v", err)
	}
	want := []recordedCommand{
		{name: "ip", args: []string{"link", "set", "dev", "subnet-a", "up"}},
		{name: "ip", args: []string{"addr", "replace", "10.220.1.1/24", "dev", "subnet-a"}},
		{name: "ip", args: []string{"route", "replace", "10.240.0.0/16", "dev", "subnet-a"}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("commands = %#v, want %#v", runner.calls, want)
	}
}

func TestReconcileRemovesStateMissingFromNewConfig(t *testing.T) {
	runner := &fakeRunner{runFailures: map[string]error{}}
	previous := RouterConfig{
		GatewayIP: []GatewayIPSpec{{Interface: "subnet-a", Address: "10.220.1.1/24"}},
		Routes:    []RouteSpec{{Destination: "10.240.0.0/16", Interface: "subnet-a"}},
	}
	if err := reconcile(context.Background(), runner, previous, RouterConfig{}); err != nil {
		t.Fatalf("reconcile() error = %v", err)
	}
	want := []recordedCommand{
		{name: "ip", args: []string{"addr", "del", "10.220.1.1/24", "dev", "subnet-a"}},
		{name: "ip", args: []string{"route", "del", "10.240.0.0/16", "dev", "subnet-a"}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("commands = %#v, want %#v", runner.calls, want)
	}
}

func TestReconcileRetriesWhenInterfaceIsMissing(t *testing.T) {
	runner := &fakeRunner{runFailures: map[string]error{
		"ip link set dev subnet-a up": errors.New("Cannot find device"),
	}}
	desired := RouterConfig{GatewayIP: []GatewayIPSpec{{Interface: "subnet-a", Address: "10.220.1.1/24"}}}
	if err := reconcile(context.Background(), runner, RouterConfig{}, desired); err == nil {
		t.Fatal("reconcile() succeeded for a missing interface")
	}
	if len(runner.calls) != 1 {
		t.Fatalf("commands = %#v, want only the interface-up command", runner.calls)
	}
}
