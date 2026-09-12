package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck
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

func TestRouterAgent(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Router Agent Suite")
}

var _ = Describe("Router agent configuration", func() {
	Describe("parseConfig", func() {
		It("parses gateway and route state", func() {
			config, err := parseConfig([]byte(`{
        "version":"2",
        "gatewayIPs":[{"interface":"subnet-a","address":"10.220.1.1/24"}],
        "routes":[{"destination":"10.240.0.0/16","interface":"subnet-a"}]
    }`))

			Expect(err).NotTo(HaveOccurred())
			Expect(config.Version).To(Equal("2"))
			Expect(config.GatewayIP).To(HaveLen(1))
			Expect(config.Routes).To(HaveLen(1))
		})

		It("rejects an incomplete route", func() {
			_, err := parseConfig([]byte(`{"gatewayIPs":[],"routes":[{"destination":"10.0.0.0/8"}]}`))

			Expect(err).To(MatchError(ContainSubstring("routes[0].interface is required")))
		})
	})

	Describe("reconcile", func() {
		It("applies desired addresses and routes", func() {
			runner := &fakeRunner{runFailures: map[string]error{}}
			desired := RouterConfig{
				GatewayIP: []GatewayIPSpec{{Interface: "subnet-a", Address: "10.220.1.1/24"}},
				Routes:    []RouteSpec{{Destination: "10.240.0.0/16", Interface: "subnet-a"}},
			}

			err := reconcile(context.Background(), runner, RouterConfig{}, desired)
			Expect(err).NotTo(HaveOccurred())
			Expect(runner.calls).To(Equal([]recordedCommand{
				{name: "ip", args: []string{"link", "set", "dev", "subnet-a", "up"}},
				{name: "ip", args: []string{"addr", "replace", "10.220.1.1/24", "dev", "subnet-a"}},
				{name: "ip", args: []string{"route", "replace", "10.240.0.0/16", "dev", "subnet-a"}},
			}))
		})

		It("removes state missing from the new config", func() {
			runner := &fakeRunner{runFailures: map[string]error{}}
			previous := RouterConfig{
				GatewayIP: []GatewayIPSpec{{Interface: "subnet-a", Address: "10.220.1.1/24"}},
				Routes:    []RouteSpec{{Destination: "10.240.0.0/16", Interface: "subnet-a"}},
			}

			err := reconcile(context.Background(), runner, previous, RouterConfig{})
			Expect(err).NotTo(HaveOccurred())
			Expect(runner.calls).To(Equal([]recordedCommand{
				{name: "ip", args: []string{"addr", "del", "10.220.1.1/24", "dev", "subnet-a"}},
				{name: "ip", args: []string{"route", "del", "10.240.0.0/16", "dev", "subnet-a"}},
			}))
		})

		It("returns an error when an interface is missing", func() {
			runner := &fakeRunner{runFailures: map[string]error{
				"ip link set dev subnet-a up": errors.New("Cannot find device"),
			}}
			desired := RouterConfig{GatewayIP: []GatewayIPSpec{{Interface: "subnet-a", Address: "10.220.1.1/24"}}}

			err := reconcile(context.Background(), runner, RouterConfig{}, desired)
			Expect(err).To(HaveOccurred())
			Expect(runner.calls).To(HaveLen(1))
		})
	})
})
