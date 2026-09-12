// Router agent applies the Linux network state written by the OSAC operator to
// the router pod. It deliberately has no Kubernetes or OVN client: the
// ConfigMap is projected into the container as a file and is polled so that
// updates are noticed without restarting the pod.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	defaultConfigFile = "/etc/osac-router-agent/config/config.json"
	defaultPollPeriod = 2 * time.Second
)

// RouterConfig is the versioned, operator-authored Linux network contract.
// Port security is intentionally absent: the current implementation is the
// AAP cudn_net role's temporary OVN-NBDB stopgap, not an agent responsibility.
type RouterConfig struct {
	Version   string          `json:"version,omitempty"`
	GatewayIP []GatewayIPSpec `json:"gatewayIPs"`
	Routes    []RouteSpec     `json:"routes"`
}

type GatewayIPSpec struct {
	Interface string `json:"interface"`
	Address   string `json:"address"`
}

type RouteSpec struct {
	Destination string `json:"destination"`
	Interface   string `json:"interface"`
	Gateway     string `json:"gateway,omitempty"`
}

type commandRunner interface {
	Run(ctx context.Context, name string, args ...string) error
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (execCommandRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return output, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	configFile := getenv("ROUTER_POD_CONFIG_FILE", defaultConfigFile)
	pollPeriod := durationFromEnv("ROUTER_POD_CONFIG_POLL_INTERVAL", defaultPollPeriod)
	clusterInterface := getenv("ROUTER_POD_CLUSTER_NET_IFACE", "eth0")
	clusterIP := os.Getenv("ROUTER_POD_CLUSTER_NET_IP")
	if clusterIP == "" {
		log.Fatal("ROUTER_POD_CLUSTER_NET_IP must be set from the Downward API")
	}

	if err := run(ctx, execCommandRunner{}, configFile, pollPeriod, clusterInterface, clusterIP); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, runner commandRunner, configFile string, pollPeriod time.Duration, clusterInterface, clusterIP string) error {
	if err := ensureSNAT(ctx, runner, clusterInterface, clusterIP); err != nil {
		return fmt.Errorf("configure cluster-network SNAT: %w", err)
	}

	previous := RouterConfig{}
	lastHash := ""
	for {
		raw, err := os.ReadFile(configFile)
		if err != nil {
			log.Printf("waiting for router ConfigMap file %q: %v", configFile, err)
		} else {
			hash := configHash(raw)
			if hash != lastHash {
				config, parseErr := parseConfig(raw)
				if parseErr != nil {
					log.Printf("ignoring invalid router configuration: %v", parseErr)
				} else if reconcileErr := reconcile(ctx, runner, previous, config); reconcileErr != nil {
					// Do not advance lastHash: a missing interface or a transient
					// netlink error is retried on the next poll even if the ConfigMap
					// contents have not changed.
					log.Printf("router configuration is not applied yet: %v", reconcileErr)
				} else {
					previous = config
					lastHash = hash
					log.Printf("router configuration applied (version=%q)", config.Version)
				}
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pollPeriod):
		}
	}
}

func parseConfig(raw []byte) (RouterConfig, error) {
	var config RouterConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return RouterConfig{}, fmt.Errorf("parse JSON: %w", err)
	}
	if err := validateConfig(config); err != nil {
		return RouterConfig{}, err
	}
	return config, nil
}

func validateConfig(config RouterConfig) error {
	seenAddresses := make(map[string]struct{}, len(config.GatewayIP))
	for index, gateway := range config.GatewayIP {
		if gateway.Interface == "" {
			return fmt.Errorf("gatewayIPs[%d].interface is required", index)
		}
		if gateway.Address == "" {
			return fmt.Errorf("gatewayIPs[%d].address is required", index)
		}
		key := gateway.Interface + "\x00" + gateway.Address
		if _, exists := seenAddresses[key]; exists {
			return fmt.Errorf("gatewayIPs[%d] duplicates interface/address %q", index, key)
		}
		seenAddresses[key] = struct{}{}
	}

	seenRoutes := make(map[string]struct{}, len(config.Routes))
	for index, route := range config.Routes {
		if route.Destination == "" {
			return fmt.Errorf("routes[%d].destination is required", index)
		}
		if route.Interface == "" {
			return fmt.Errorf("routes[%d].interface is required", index)
		}
		key := routeKey(route)
		if _, exists := seenRoutes[key]; exists {
			return fmt.Errorf("routes[%d] duplicates route %q", index, key)
		}
		seenRoutes[key] = struct{}{}
	}
	return nil
}

// reconcile is intentionally idempotent. Desired state is applied first, and
// state present in the previous successful configuration but absent from the
// new one is removed only after all desired operations succeed. This keeps a
// ConfigMap update from leaving a partially configured router behind when a
// newly referenced Multus interface has not appeared yet.
func reconcile(ctx context.Context, runner commandRunner, previous, desired RouterConfig) error {
	if err := validateConfig(desired); err != nil {
		return err
	}

	for _, gateway := range desired.GatewayIP {
		if err := runner.Run(ctx, "ip", "link", "set", "dev", gateway.Interface, "up"); err != nil {
			return fmt.Errorf("bring interface %q up: %w", gateway.Interface, err)
		}
		if err := runner.Run(ctx, "ip", "addr", "replace", gateway.Address, "dev", gateway.Interface); err != nil {
			return fmt.Errorf("assign %s to interface %q: %w", gateway.Address, gateway.Interface, err)
		}
	}

	for _, route := range desired.Routes {
		if err := runRoute(ctx, runner, "replace", route); err != nil {
			return fmt.Errorf("install route %q: %w", routeKey(route), err)
		}
	}

	desiredAddresses := make(map[string]struct{}, len(desired.GatewayIP))
	for _, gateway := range desired.GatewayIP {
		desiredAddresses[gateway.Interface+"\x00"+gateway.Address] = struct{}{}
	}
	for _, gateway := range previous.GatewayIP {
		if _, exists := desiredAddresses[gateway.Interface+"\x00"+gateway.Address]; exists {
			continue
		}
		// The interface may already have disappeared during a live detach. A
		// failed delete is therefore deliberately non-fatal.
		if err := runner.Run(ctx, "ip", "addr", "del", gateway.Address, "dev", gateway.Interface); err != nil {
			log.Printf("could not remove stale address %s from %s: %v", gateway.Address, gateway.Interface, err)
		}
	}

	desiredRoutes := make(map[string]struct{}, len(desired.Routes))
	for _, route := range desired.Routes {
		desiredRoutes[routeKey(route)] = struct{}{}
	}
	for _, route := range previous.Routes {
		if _, exists := desiredRoutes[routeKey(route)]; exists {
			continue
		}
		if err := runRoute(ctx, runner, "del", route); err != nil {
			log.Printf("could not remove stale route %q: %v", routeKey(route), err)
		}
	}

	return nil
}

func routeCommand(operation string, route RouteSpec) []string {
	args := []string{"ip", "route", operation, route.Destination}
	if route.Gateway != "" {
		args = append(args, "via", route.Gateway)
	}
	return append(args, "dev", route.Interface)
}

func runRoute(ctx context.Context, runner commandRunner, operation string, route RouteSpec) error {
	args := routeCommand(operation, route)
	return runner.Run(ctx, args[0], args[1:]...)
}

func routeKey(route RouteSpec) string {
	return strings.Join([]string{route.Destination, route.Gateway, route.Interface}, "\x00")
}

func ensureSNAT(ctx context.Context, runner commandRunner, clusterInterface, clusterIP string) error {
	// Adding an existing nftables table/chain is harmless for this setup. Ignore
	// those two return values and let the authoritative list operation below
	// surface permission or syntax errors.
	_ = runner.Run(ctx, "nft", "add", "table", "ip", "nat")
	_ = runner.Run(ctx, "nft", "add", "chain", "ip", "nat", "postrouting", "{ type nat hook postrouting priority 100 ; }")

	rules, err := runner.Output(ctx, "nft", "list", "chain", "ip", "nat", "postrouting")
	if err != nil {
		return err
	}
	marker := fmt.Sprintf("oifname \"%s\" snat to", clusterInterface)
	if strings.Contains(string(rules), marker) {
		return nil
	}
	return runner.Run(ctx, "nft", "add", "rule", "ip", "nat", "postrouting", "oifname", clusterInterface, "snat", "to", clusterIP)
}

func configHash(raw []byte) string {
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func getenv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func durationFromEnv(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		log.Printf("invalid %s=%q, using %s", name, value, fallback)
		return fallback
	}
	return parsed
}
