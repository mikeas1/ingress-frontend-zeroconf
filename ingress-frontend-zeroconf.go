package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	v1 "k8s.io/api/core/v1"
	v1beta1 "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/fields"

	docopt "github.com/docopt/docopt-go"
	"github.com/grandcat/zeroconf"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

// LocalHostname An Ingress hostname in the .local domain
type LocalHostname struct {
	TLS      bool
	Hostname string
}

func main() {
	usage := `Kubernetes Ingress Frontend Zeroconf - Broadcast ingress hostnames via mDNS

Usage: broadcast [options]

Options:
  --interface=name  Interface on which to broadcast [default: eth0]
  --kubeconfig      Use $HOME/.kube config instead of in-cluster config
  --debug           Print debugging information
  -h, --help        show this help`

	arguments, _ := docopt.ParseDoc(usage)
	debug, _ := arguments.Bool("--debug")
	if debug {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	log.Debug(arguments)

	interfaceName, err := arguments.String("--interface")
	if err != nil {
		log.Fatalf("retrieving interface arg: %+v", err)
	}
	broadcastInterface, err := getInterfaceByName(interfaceName)
	if err != nil {
		log.Fatalf("Setting up interface: %+v", err)
	}

	useKubeConfig, err := arguments.Bool("--kubeconfig")
	if err != nil {
		log.Fatalf("retrieving kubeconfig arg: %+v", err)
	}
	clientset := getKubernetesClientSet(useKubeConfig)

	var zeroconfServers = map[LocalHostname]*zeroconf.Server{}
	defer unregisterAllHostnames(zeroconfServers)
	watcher := cache.NewListWatchFromClient(clientset.ExtensionsV1beta1().RESTClient(), "ingresses", v1.NamespaceAll, fields.Everything())
	log.Debugf("Watching ingresses")
	_, controller := cache.NewInformer(watcher, &v1beta1.Ingress{}, time.Second*30, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			hostnames, ingressIP := getIngressHostnames(obj.(*v1beta1.Ingress))
			registerHostnames(hostnames, broadcastInterface, ingressIP, zeroconfServers)
		},
		DeleteFunc: func(obj interface{}) {
			hostnames, _ := getIngressHostnames(obj.(*v1beta1.Ingress))
			unregisterHostnames(hostnames, zeroconfServers)
		},
		UpdateFunc: func(oldObj interface{}, newObj interface{}) {
			oldIngress := oldObj.(*v1beta1.Ingress)
			newIngress := oldObj.(*v1beta1.Ingress)
			oldHostnames, _ := getIngressHostnames(oldIngress)
			newHostnames, ingressIP := getIngressHostnames(newIngress)
			if !reflect.DeepEqual(oldHostnames, newHostnames) {
				log.Infof("Ingress %v changed, re-registering hostnames", oldIngress.Name)
				unregisterHostnames(oldHostnames, zeroconfServers)
				registerHostnames(newHostnames, broadcastInterface, ingressIP, zeroconfServers)
			}
		},
	})

	sigs := make(chan os.Signal, 1)
	stop := make(chan struct{})
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	go controller.Run(stop)

	go func() {
		sig := <-sigs
		log.Debugf("%v", sig)
		close(stop)
	}()
	<-stop
}

func getInterfaceByName(interfaceName string) (net.Interface, error) {
	ifaces, _ := net.Interfaces()
	ifaceNames := []string{}
	for _, iface := range ifaces {
		if iface.Name == interfaceName {
			log.Debugf("Found interface %v", interfaceName)
			return iface, nil
		}
		ifaceNames = append(ifaceNames, iface.Name)
	}
	return net.Interface{}, fmt.Errorf("No interface named %v was found, available interfaces are:\n%v", interfaceName, strings.Join(ifaceNames, "\n"))
}

func getKubernetesClientSet(useKubeConfig bool) *kubernetes.Clientset {
	var config *rest.Config
	var err error
	if useKubeConfig {
		var home string
		if home = os.Getenv("HOME"); home == "" {
			home = os.Getenv("USERPROFILE") // windows
		}
		path := filepath.Join(home, ".kube", "config")
		config, err = clientcmd.BuildConfigFromFlags("", path)
		if err != nil {
			log.Fatalf("failed to construct kube client config from path %v: %+v", path, err)
		}
	} else {
		config, err = rest.InClusterConfig()
		if err != nil {
			log.Fatalf("failed to construct in-cluster kube config: %+v", err)
		}
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("failed to construct kube client: %+v", err)
	}
	return clientset
}

func registerHostnames(
	hostnames []LocalHostname,
	broadcastInterface net.Interface,
	ingressIP net.IP,
	servers map[LocalHostname]*zeroconf.Server) {
	for _, local := range hostnames {
		log.Infof("Registering %v", local.Hostname)
		// Simplification: Assume ingress listens on standard HTTP(s) ports.
		port := 80
		if local.TLS {
			port = 443
		}
		server, err := zeroconf.RegisterProxy(
			local.Hostname,
			"_http._tcp.",
			"local.",
			port,
			local.Hostname,
			[]string{ingressIP.String()},
			[]string{"path=/"},
			[]net.Interface{broadcastInterface},
		)
		if err != nil {
			log.Errorf("Failed to register hostname %v: %+v", local.Hostname, err)
			continue
		}
		servers[local] = server
	}
}

func unregisterHostnames(hostnames []LocalHostname, servers map[LocalHostname]*zeroconf.Server) {
	for _, local := range hostnames {
		if server, exists := servers[local]; exists {
			log.Infof("Unregistering %v", local.Hostname)
			server.Shutdown()
			delete(servers, local)
		}
	}
}

func unregisterAllHostnames(servers map[LocalHostname]*zeroconf.Server) {
	for local, server := range servers {
		log.Infof("Unregistering %v", local.Hostname)
		server.Shutdown()
	}
}

func getIngressHostnames(ingress *v1beta1.Ingress) ([]LocalHostname, net.IP) {
	// The same ingress can have both cleartext and tls hosts.
	// This is not implemented yet, for now we just check for the presence
	// of the tls.
	tls := ingress.Spec.TLS != nil
	hostnames := []LocalHostname{}
	for _, rule := range ingress.Spec.Rules {
		hostname := rule.Host
		if !strings.HasSuffix(hostname, ".local") {
			continue
		}
		hostnames = append(hostnames, LocalHostname{tls, strings.TrimSuffix(hostname, ".local")})
	}
	ip := net.ParseIP(ingress.Status.LoadBalancer.Ingress[0].IP)
	return hostnames, ip
}
