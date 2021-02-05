package agent

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/cgroups"
	cgroupsv2 "github.com/containerd/cgroups/v2"
	systemd "github.com/coreos/go-systemd/daemon"
	"github.com/rancher/k3s/pkg/agent/config"
	"github.com/rancher/k3s/pkg/agent/containerd"
	"github.com/rancher/k3s/pkg/agent/flannel"
	"github.com/rancher/k3s/pkg/agent/netpol"
	"github.com/rancher/k3s/pkg/agent/proxy"
	"github.com/rancher/k3s/pkg/agent/syssetup"
	"github.com/rancher/k3s/pkg/agent/tunnel"
	"github.com/rancher/k3s/pkg/cli/cmds"
	"github.com/rancher/k3s/pkg/clientaccess"
	"github.com/rancher/k3s/pkg/daemons/agent"
	daemonconfig "github.com/rancher/k3s/pkg/daemons/config"
	"github.com/rancher/k3s/pkg/nodeconfig"
	"github.com/rancher/k3s/pkg/rootless"
	"github.com/rancher/k3s/pkg/version"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	InternalIPLabel = version.Program + ".io/internal-ip"
	ExternalIPLabel = version.Program + ".io/external-ip"
	HostnameLabel   = version.Program + ".io/hostname"
)

const (
	dockershimSock = "unix:///var/run/dockershim.sock"
	containerdSock = "unix:///run/k3s/containerd/containerd.sock"
)

// setupCriCtlConfig creates the crictl config file and populates it
// with the given data from config.
func setupCriCtlConfig(cfg cmds.Agent, nodeConfig *daemonconfig.Node) error {
	cre := nodeConfig.ContainerRuntimeEndpoint
	if cre == "" {
		switch {
		case cfg.Docker:
			cre = dockershimSock
		default:
			cre = containerdSock
		}
	}

	agentConfDir := filepath.Join(cfg.DataDir, "agent", "etc")
	if _, err := os.Stat(agentConfDir); os.IsNotExist(err) {
		if err := os.MkdirAll(agentConfDir, 0700); err != nil {
			return err
		}
	}

	crp := "runtime-endpoint: " + cre + "\n"
	return ioutil.WriteFile(agentConfDir+"/crictl.yaml", []byte(crp), 0600)
}

func run(ctx context.Context, cfg *cmds.Agent, proxy proxy.Proxy) error {
	nodeConfig := config.Get(ctx, *cfg, proxy)

	if err := setupCriCtlConfig(*cfg, nodeConfig); err != nil {
		return err
	}

	if !nodeConfig.NoFlannel {
		if err := flannel.Prepare(ctx, nodeConfig); err != nil {
			return err
		}
	}

	if !nodeConfig.Docker && nodeConfig.ContainerRuntimeEndpoint == "" {
		if err := containerd.Run(ctx, nodeConfig); err != nil {
			return err
		}
	}
	if err := setupTunnelAndRunAgent(ctx, nodeConfig, cfg, proxy); err != nil {
		return err
	}

	coreClient, err := coreClient(nodeConfig.AgentConfig.KubeConfigKubelet)
	if err != nil {
		return err
	}
	if !nodeConfig.NoFlannel {
		if err := flannel.Run(ctx, nodeConfig, coreClient.CoreV1().Nodes()); err != nil {
			return err
		}
	}

	if err := configureNode(ctx, &nodeConfig.AgentConfig, coreClient.CoreV1().Nodes()); err != nil {
		return err
	}

	if !nodeConfig.AgentConfig.DisableNPC {
		if err := netpol.Run(ctx, nodeConfig); err != nil {
			return err
		}
	}

	<-ctx.Done()
	return ctx.Err()
}

func coreClient(cfg string) (kubernetes.Interface, error) {
	restConfig, err := clientcmd.BuildConfigFromFlags("", cfg)
	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(restConfig)
}

func Run(ctx context.Context, cfg *cmds.Agent) error {
	if err := validate(); err != nil {
		return err
	}
	syssetup.Configure()

	if cfg.Rootless && !cfg.RootlessAlreadyUnshared {
		if err := rootless.Rootless(cfg.DataDir); err != nil {
			return err
		}
	}

	agentDir := filepath.Join(cfg.DataDir, "agent")
	if err := os.MkdirAll(agentDir, 0700); err != nil {
		return err
	}

	proxy, err := proxy.NewAPIProxy(!cfg.DisableLoadBalancer, cfg.DataDir, cfg.ServerURL, cfg.LBServerPort)
	if err != nil {
		return err
	}

	for {
		newToken, err := clientaccess.ParseAndValidateTokenForUser(proxy.SupervisorURL(), cfg.Token, "node")
		if err != nil {
			logrus.Error(err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		cfg.Token = newToken.String()
		break
	}
	systemd.SdNotify(true, "READY=1\n")
	return run(ctx, cfg, proxy)
}

func validate() error {
	if cgroups.Mode() == cgroups.Unified {
		return validateCgroupsV2()
	}
	return validateCgroupsV1()
}

func validateCgroupsV1() error {
	cgroups, err := ioutil.ReadFile("/proc/self/cgroup")
	if err != nil {
		return err
	}

	if !strings.Contains(string(cgroups), "cpuset") {
		logrus.Warn(`Failed to find cpuset cgroup, you may need to add "cgroup_enable=cpuset" to your linux cmdline (/boot/cmdline.txt on a Raspberry Pi)`)
	}

	if !strings.Contains(string(cgroups), "memory") {
		msg := "ailed to find memory cgroup, you may need to add \"cgroup_memory=1 cgroup_enable=memory\" to your linux cmdline (/boot/cmdline.txt on a Raspberry Pi)"
		logrus.Error("F" + msg)
		return errors.New("f" + msg)
	}

	return nil
}

func validateCgroupsV2() error {
	manager, err := cgroupsv2.LoadManager("/sys/fs/cgroup", "/")
	if err != nil {
		return err
	}
	controllers, err := manager.RootControllers()
	if err != nil {
		return err
	}
	m := make(map[string]struct{})
	for _, controller := range controllers {
		m[controller] = struct{}{}
	}
	for _, controller := range []string{"cpu", "cpuset", "memory"} {
		if _, ok := m[controller]; !ok {
			return fmt.Errorf("failed to find %s cgroup (v2)", controller)
		}
	}
	return nil
}

func configureNode(ctx context.Context, agentConfig *daemonconfig.Agent, nodes v1.NodeInterface) error {
	count := 0
	for {
		node, err := nodes.Get(ctx, agentConfig.NodeName, metav1.GetOptions{})
		if err != nil {
			if count%30 == 0 {
				logrus.Infof("Waiting for kubelet to be ready on node %s: %v", agentConfig.NodeName, err)
			}
			count++
			time.Sleep(1 * time.Second)
			continue
		}

		newLabels, updateMutables := updateMutableLabels(agentConfig, node.Labels)

		updateAddresses := !agentConfig.DisableCCM
		if updateAddresses {
			newLabels, updateAddresses = updateAddressLabels(agentConfig, newLabels)
		}

		// inject node config
		updateNode, err := nodeconfig.SetNodeConfigAnnotations(node)
		if err != nil {
			return err
		}
		if updateAddresses || updateMutables {
			node.Labels = newLabels
			updateNode = true
		}
		if updateNode {
			if _, err := nodes.Update(ctx, node, metav1.UpdateOptions{}); err != nil {
				logrus.Infof("Failed to update node %s: %v", agentConfig.NodeName, err)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Second):
					continue
				}
			}
			logrus.Infof("labels have been set successfully on node: %s", agentConfig.NodeName)
		} else {
			logrus.Infof("labels have already set on node: %s", agentConfig.NodeName)
		}

		break
	}

	return nil
}

func updateMutableLabels(agentConfig *daemonconfig.Agent, nodeLabels map[string]string) (map[string]string, bool) {
	result := map[string]string{}

	for _, m := range agentConfig.NodeLabels {
		var (
			v string
			p = strings.SplitN(m, `=`, 2)
			k = p[0]
		)
		if len(p) > 1 {
			v = p[1]
		}
		result[k] = v
	}
	result = labels.Merge(nodeLabels, result)
	return result, !equality.Semantic.DeepEqual(nodeLabels, result)
}

func updateAddressLabels(agentConfig *daemonconfig.Agent, nodeLabels map[string]string) (map[string]string, bool) {
	result := map[string]string{
		InternalIPLabel: agentConfig.NodeIP,
		HostnameLabel:   agentConfig.NodeName,
	}

	if agentConfig.NodeExternalIP != "" {
		result[ExternalIPLabel] = agentConfig.NodeExternalIP
	}

	result = labels.Merge(nodeLabels, result)
	return result, !equality.Semantic.DeepEqual(nodeLabels, result)
}

// setupTunnelAndRunAgent should start the setup tunnel before starting kubelet and kubeproxy
// there are special case for etcd agents, it will wait until it can find the serverURLch stub
// and update the proxy with the servers addresses, if in rke2 we need to start the agent before
// the tunnel is setup to allow kubelet to start first and start the pods
func setupTunnelAndRunAgent(ctx context.Context, nodeConfig *daemonconfig.Node, cfg *cmds.Agent, proxy proxy.Proxy) error {
	var agentRan bool
	if cfg.ETCDAgent {
		// only in rke2 run the agent before the tunnel setup and check for that later in the function
		if proxy.IsAPIServerLBEnabled() {
			if err := agent.Agent(&nodeConfig.AgentConfig); err != nil {
				return err
			}
			agentRan = true
		}
		for {
			time.Sleep(5 * time.Second)
			if cfg.ServerURLTmp != "" {
				cfg.ServerURL = cfg.ServerURLTmp
				u, err := url.Parse(cfg.ServerURL)
				if err != nil {
					logrus.Warn(err)
					continue
				}
				proxy.Update([]string{fmt.Sprintf("%s:%d", u.Hostname(), nodeConfig.ServerHTTPSPort)})
				break
			}
		}
	}

	if err := tunnel.Setup(ctx, nodeConfig, proxy); err != nil {
		return err
	}
	if !agentRan {
		return agent.Agent(&nodeConfig.AgentConfig)
	}
	return nil
}
