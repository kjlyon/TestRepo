/*
http://www.apache.org/licenses/LICENSE-2.0.txt


Copyright 2016 Intel Corporation

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package plugin

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"log"

	"runtime"

	"github.com/intelsdi-x/snap-plugin-lib-go/v1/plugin/rpc"
	"github.com/urfave/cli"
	"google.golang.org/grpc"
)

// Plugin is the base plugin type. All plugins must implement GetConfigPolicy.
type Plugin interface {
	GetConfigPolicy() (ConfigPolicy, error)
}

// Collector is a plugin which is the source of new data in the Snap pipeline.
type Collector interface {
	Plugin

	GetMetricTypes(Config) ([]Metric, error)
	CollectMetrics([]Metric) ([]Metric, error)
}

// Processor is a plugin which filters, agregates, or decorates data in the
// Snap pipeline.
type Processor interface {
	Plugin

	Process([]Metric, Config) ([]Metric, error)
}

// Publisher is a sink in the Snap pipeline.  It publishes data into another
// System, completing a Workflow path.
type Publisher interface {
	Plugin

	Publish([]Metric, Config) error
}

// StartCollector is given a Collector implementation and its metadata,
// generates a response for the initial stdin / stdout handshake, and starts
// the plugin's gRPC server.
func StartCollector(plugin Collector, name string, version int, opts ...MetaOpt) int {
	m := newMeta(collectorType, name, version, opts...)
	server := grpc.NewServer()
	// TODO(danielscottt) SSL
	proxy := &collectorProxy{
		plugin:      plugin,
		pluginProxy: *newPluginProxy(plugin),
	}
	rpc.RegisterCollectorServer(server, proxy)
	return startPlugin(server, m, &proxy.pluginProxy)
}

// StartProcessor is given a Processor implementation and its metadata,
// generates a response for the initial stdin / stdout handshake, and starts
// the plugin's gRPC server.
func StartProcessor(plugin Processor, name string, version int, opts ...MetaOpt) int {
	m := newMeta(processorType, name, version, opts...)
	server := grpc.NewServer()
	// TODO(danielscottt) SSL
	proxy := &processorProxy{
		plugin:      plugin,
		pluginProxy: *newPluginProxy(plugin),
	}
	rpc.RegisterProcessorServer(server, proxy)
	return startPlugin(server, m, &proxy.pluginProxy)
}

// StartPublisher is given a Publisher implementation and its metadata,
// generates a response for the initial stdin / stdout handshake, and starts
// the plugin's gRPC server.
func StartPublisher(plugin Publisher, name string, version int, opts ...MetaOpt) int {
	m := newMeta(publisherType, name, version, opts...)
	server := grpc.NewServer()
	// TODO(danielscottt) SSL
	proxy := &publisherProxy{
		plugin:      plugin,
		pluginProxy: *newPluginProxy(plugin),
	}
	rpc.RegisterPublisherServer(server, proxy)
	return startPlugin(server, m, &proxy.pluginProxy)
}

type server interface {
	Serve(net.Listener) error
}

type preamble struct {
	Meta          meta
	ListenAddress string
	PprofAddress  string
	Type          pluginType
	State         int
	ErrorMessage  string
}

func startPlugin(srv server, m meta, p *pluginProxy) int {
	app := cli.NewApp()
	app.Name = m.Name
	app.Version = strconv.Itoa(m.Version)
	app.Usage = "A Snap " + getPluginType(m.Type) + " plugin"
	app.Flags = []cli.Flag{flConfig, flPort, flPingTimeout, flPprof}

	app.Action = func(c *cli.Context) error {
		if c.NArg() > 0 {
			printPreamble(srv, m, p)
		} else { //implies run diagnostics
			var c Config
			if config != "" {
				byteArray := []byte(config)
				err := json.Unmarshal(byteArray, &c)
				if err != nil {
					return fmt.Errorf("There was an error when parsing config. Please ensure your config is valid. \n %v", err)
				}
			}

			switch p.plugin.(type) {
			case Collector:
				showDiagnostics(m, p, c)
			case Processor:
				fmt.Println("Diagnostics not currently available for processor plugins.")
			case Publisher:
				fmt.Println("Diagnostics not currently available for publisher plugins.")
			}
		}
		if Pprof {
			return getPort()
		}

		return nil
	}
	app.Run(os.Args)

	return 0
}

func printPreamble(srv server, m meta, p *pluginProxy) error {
	l, err := net.Listen("tcp", ":"+listenPort)
	if err != nil {
		panic("Unable to get open port")
	}
	l.Close()

	addr := fmt.Sprintf("127.0.0.1:%v", l.Addr().(*net.TCPAddr).Port)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		// TODO(danielscottt): logging
		panic(err)
	}
	go func() {
		err := srv.Serve(lis)
		if err != nil {
			panic(err)
		}
	}()
	resp := preamble{
		Meta:          m,
		ListenAddress: addr,
		Type:          m.Type,
		PprofAddress:  pprofPort,
		State:         0, // Hardcode success since panics on err
	}
	preambleJSON, err := json.Marshal(resp)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(preambleJSON))
	go p.HeartbeatWatch()
	// TODO(danielscottt): exit code
	<-p.halt

	return nil
}

// GetPluginType converts a pluginType to a string
// describing what type of plugin this is
func getPluginType(plType pluginType) string {
	switch plType {
	case collectorType:
		return "collector"

	case publisherType:
		return "publisher"

	case processorType:
		return "processor"
	}
	return ""
}

// GetPluginType converts a pluginType to a string
// as described in snap/control/plugin/plugin.go
func getRPCType(i int) string {
	if i == 0 {
		return "NativeRPC (0)"
	} else if i == 2 {
		return "GRPC (2)"
	}
	return "Not a valid RPCType"
}

func showDiagnostics(m meta, p *pluginProxy, c Config) error {
	defer timeTrack(time.Now(), "showDiagnostics")
	printRuntimeDetails(m)
	met := printMetricTypes(p, c)

	printCollectMetrics(p, met)
	printContactUs()
	return nil
}

func printMetricTypes(p *pluginProxy, conf Config) []Metric {
	defer timeTrack(time.Now(), "printMetricTypes")
	met, err := p.plugin.(Collector).GetMetricTypes(conf)
	if err != nil {
		log.Printf("There was an error in the call to GetMetricTypes. Please ensure your config contains any required fields mentioned in the error below. \n %v", err)
		os.Exit(1)
	}
	//apply any config passed in to met so that
	//CollectMetrics can see the config for each metric
	for i := range met {
		met[i].Config = conf

		// //check to ensure config got put in
		// for k, v := range j.Config {
		// 	switch vv := v.(type) {
		// 	case string:
		// 		fmt.Println(k, "is string", vv)
		// 	case int:
		// 		fmt.Println(k, "is int", vv)
		// 	case bool:
		// 		fmt.Println(k, "is a bool", vv)
		// 	case []interface{}:
		// 		fmt.Println(k, "is an array:")
		// 		for i, u := range vv {
		// 			fmt.Println(i, u)
		// 		}
		// 	default:
		// 		fmt.Println(k, "is of a type I don't know how to handle")
		// 	}
		// }

	}

	fmt.Println("Metric catalog will be updated to include: ")

	for _, j := range met {
		fmt.Printf("    Namespace: %v \n", j.Namespace.String())
	}
	return met
}

func printCollectMetrics(p *pluginProxy, m []Metric) {
	defer timeTrack(time.Now(), "printCollectMetrics")
	cltd, err := p.plugin.(Collector).CollectMetrics(m)
	if err != nil {
		log.Printf("There was an error in the call to CollectMetrics. Please ensure your config contains any required fields mentioned in the error below. \n %v", err)
		os.Exit(1)
	}
	fmt.Println("Metrics that can be collected right now are: ")
	for _, j := range cltd {
		fmt.Printf("    Namespace: %-30v  Value Type: %-10T  Value: %v \n", j.Namespace, j.Data, j.Data)
	}
}

func printRuntimeDetails(m meta) {
	defer timeTrack(time.Now(), "printRuntimeDetails")
	fmt.Printf("Runtime Details:\n    PluginName: %v, Version: %v \n    RPC Type: %v, RPC Version: %v \n", m.Name, m.Version, getRPCType(m.RPCType), m.RPCVersion)

	fmt.Printf("    Operating system: %v \n    Architecture: %v \n    Go version: %v \n", runtime.GOOS, runtime.GOARCH, runtime.Version())

}

func printContactUs() {
	fmt.Print("Thank you for using this Snap plugin. If you have questions or are running \ninto errors, please contact us on Github (github.com/intelsdi-x/snap) or \nour Slack channel (intelsdi-x.herokuapp.com). \nThe repo for this plugin can be found: github.com/intelsdi-x/___??___. \nWhen submitting a new issue on Github, please include this diagnostic \nprint out so that we have a starting point for addressing your question. \nThank you. \n\n")
}

func timeTrack(start time.Time, name string) {
	elapsed := time.Since(start)
	fmt.Printf("%s took %s \n\n", name, elapsed)
}
