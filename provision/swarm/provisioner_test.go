// Copyright 2016 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package swarm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/swarm"
	"github.com/fsouza/go-dockerclient"
	"github.com/fsouza/go-dockerclient/testing"
	"github.com/pkg/errors"
	"github.com/tsuru/config"
	"github.com/tsuru/tsuru/app"
	"github.com/tsuru/tsuru/app/image"
	"github.com/tsuru/tsuru/event"
	"github.com/tsuru/tsuru/permission"
	"github.com/tsuru/tsuru/provision"
	"github.com/tsuru/tsuru/provision/nodecontainer"
	"github.com/tsuru/tsuru/provision/provisiontest"
	"github.com/tsuru/tsuru/safe"
	"gopkg.in/check.v1"
)

func (s *S) TestInitialize(c *check.C) {
	config.Set("swarm:swarm-port", 0)
	err := s.p.Initialize()
	c.Assert(err, check.IsNil)
	c.Assert(swarmConfig.swarmPort, check.Equals, 0)
	config.Unset("swarm:swarm-port")
	err = s.p.Initialize()
	c.Assert(err, check.IsNil)
	c.Assert(swarmConfig.swarmPort, check.Equals, 2377)
	config.Unset("swarm:swarm-port")
	err = s.p.Initialize()
	c.Assert(err, check.IsNil)
	c.Assert(swarmConfig.swarmPort, check.Equals, 2377)
	// TODO(cezarsa): check TLSConfig loading
}

func (s *S) TestProvision(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", Platform: "whitespace", TeamOwner: s.team.Name}
	err = s.p.Provision(a)
	c.Assert(err, check.IsNil)
	cli, err := newClient(srv.URL())
	c.Assert(err, check.IsNil)
	nets, err := cli.ListNetworks()
	c.Assert(err, check.IsNil)
	c.Assert(nets, check.HasLen, 1)
	expected := docker.Network{ID: nets[0].ID, Name: "app-myapp-overlay", Driver: "overlay"}
	c.Assert(nets, check.DeepEquals, []docker.Network{expected})
}

func (s *S) TestAddNode(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	metadata := map[string]string{"m1": "v1", "m2": "v2", "pool": "p1"}
	opts := provision.AddNodeOptions{
		Address:  srv.URL(),
		Metadata: metadata,
	}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	node, err := s.p.GetNode(srv.URL())
	c.Assert(err, check.IsNil)
	c.Assert(node.Address(), check.Equals, srv.URL())
	c.Assert(node.Metadata(), check.DeepEquals, metadata)
	c.Assert(node.Pool(), check.Equals, "p1")
	c.Assert(node.Status(), check.Equals, "ready")
	coll, err := nodeAddrCollection()
	c.Assert(err, check.IsNil)
	defer coll.Close()
	var nodeAddrs NodeAddrs
	err = coll.FindId(uniqueDocumentID).One(&nodeAddrs)
	c.Assert(err, check.IsNil)
	c.Assert(nodeAddrs.Addresses, check.DeepEquals, []string{srv.URL()})
}

func (s *S) TestAddNodeAlreadyInSwarm(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	cli, err := docker.NewClient(srv.URL())
	c.Assert(err, check.IsNil)
	err = initSwarm(cli, srv.URL())
	c.Assert(err, check.IsNil)
	metadata := map[string]string{"m1": "v1", "m2": "v2", "pool": "p1"}
	opts := provision.AddNodeOptions{
		Address:  srv.URL(),
		Metadata: metadata,
	}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	node, err := s.p.GetNode(srv.URL())
	c.Assert(err, check.IsNil)
	c.Assert(node.Address(), check.Equals, srv.URL())
	c.Assert(node.Metadata(), check.DeepEquals, metadata)
	c.Assert(node.Pool(), check.Equals, "p1")
	c.Assert(node.Status(), check.Equals, "ready")
	coll, err := nodeAddrCollection()
	c.Assert(err, check.IsNil)
	defer coll.Close()
	var nodeAddrs NodeAddrs
	err = coll.FindId(uniqueDocumentID).One(&nodeAddrs)
	c.Assert(err, check.IsNil)
	c.Assert(nodeAddrs.Addresses, check.DeepEquals, []string{srv.URL()})
}

func (s *S) TestAddNodeMultiple(c *check.C) {
	for i := 0; i < 5; i++ {
		srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
		c.Assert(err, check.IsNil)
		defer srv.Stop()
		metadata := map[string]string{"count": fmt.Sprintf("%d", i), "pool": "p1"}
		opts := provision.AddNodeOptions{
			Address:  srv.URL(),
			Metadata: metadata,
		}
		err = s.p.AddNode(opts)
		c.Assert(err, check.IsNil, check.Commentf("server %d", i))
	}
	nodes, err := s.p.ListNodes(nil)
	c.Assert(err, check.IsNil)
	c.Assert(nodes, check.HasLen, 5)
	for i, n := range nodes {
		c.Assert(n.Metadata(), check.DeepEquals, map[string]string{
			"count": fmt.Sprintf("%d", i),
			"pool":  "p1",
		})
	}
}

func (s *S) TestAddNodeMultipleRoleCheck(c *check.C) {
	for i := 0; i < 15; i++ {
		srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
		c.Assert(err, check.IsNil)
		defer srv.Stop()
		metadata := map[string]string{"pool": "p1"}
		opts := provision.AddNodeOptions{
			Address:  srv.URL(),
			Metadata: metadata,
		}
		err = s.p.AddNode(opts)
		c.Assert(err, check.IsNil, check.Commentf("server %d", i))
	}
	cli, err := chooseDBSwarmNode()
	c.Assert(err, check.IsNil)
	nodes, err := cli.ListNodes(docker.ListNodesOptions{})
	c.Assert(err, check.IsNil)
	c.Assert(nodes, check.HasLen, 15)
	managers := 0
	for _, n := range nodes {
		if n.Spec.Role == swarm.NodeRoleManager {
			managers++
		}
	}
	c.Assert(managers, check.Equals, 7)
}

func (s *S) TestAddNodeTLS(c *check.C) {
	caPath := tmpFileWith(c, testCA)
	certPath := tmpFileWith(c, testServerCert)
	keyPath := tmpFileWith(c, testServerKey)
	defer os.Remove(certPath)
	defer os.Remove(keyPath)
	defer os.Remove(caPath)
	srv, err := testing.NewTLSServer("127.0.0.1:0", nil, nil, testing.TLSConfig{
		RootCAPath:  caPath,
		CertPath:    certPath,
		CertKeyPath: keyPath,
	})
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	url := srv.URL()
	url = strings.Replace(url, "http://", "https://", 1)
	metadata := map[string]string{"m1": "v1", "m2": "v2", "pool": "p1"}
	opts := provision.AddNodeOptions{
		Address:    url,
		Metadata:   metadata,
		CaCert:     testCA,
		ClientCert: testCert,
		ClientKey:  testKey,
	}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	node, err := s.p.GetNode(url)
	c.Assert(err, check.IsNil)
	c.Assert(node.Address(), check.Equals, url)
	c.Assert(node.Metadata(), check.DeepEquals, metadata)
	c.Assert(node.Pool(), check.Equals, "p1")
	c.Assert(node.Status(), check.Equals, "ready")
}

func (s *S) TestAddNodeFirstNodeStartsNodeContainers(c *check.C) {
	var srv1Calls []string
	srv, err := testing.NewServer("127.0.0.1:0", nil, func(r *http.Request) {
		srv1Calls = append(srv1Calls, r.Method+" "+r.URL.Path)
	})
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	srv2, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv2.Stop()
	c1 := nodecontainer.NodeContainerConfig{
		Name: "bs",
		Config: docker.Config{
			Image: "bsimg",
		},
	}
	err = nodecontainer.AddNewContainer("", &c1)
	c.Assert(err, check.IsNil)
	err = nodecontainer.AddNewContainer("p1", &c1)
	c.Assert(err, check.IsNil)
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	c.Assert(srv1Calls, check.HasLen, 10)
	c.Assert(srv1Calls[6:], check.DeepEquals, []string{
		"GET /services/node-container-bs-all",
		"POST /services/create",
		"GET /services/node-container-bs-p1",
		"POST /services/create",
	})
	client, err := docker.NewClient(srv.URL())
	c.Assert(err, check.IsNil)
	services, err := client.ListServices(docker.ListServicesOptions{})
	c.Assert(err, check.IsNil)
	c.Assert(len(services), check.Equals, 2)
	opts = provision.AddNodeOptions{Address: srv2.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	c.Assert(srv1Calls[len(srv1Calls)-1], check.Equals, "GET /nodes")
	services, err = client.ListServices(docker.ListServicesOptions{})
	c.Assert(err, check.IsNil)
	c.Assert(len(services), check.Equals, 2)
}

func (s *S) TestListNodes(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	srv2, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv2.Stop()
	metadata := map[string]string{"m1": "v1", "pool": "p1"}
	opts := provision.AddNodeOptions{
		Address:  srv.URL(),
		Metadata: metadata,
	}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	opts.Address = srv2.URL()
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	nodes, err := s.p.ListNodes(nil)
	c.Assert(err, check.IsNil)
	c.Assert(nodes, check.HasLen, 2)
	nodes, err = s.p.ListNodes([]string{srv.URL()})
	c.Assert(err, check.IsNil)
	c.Assert(nodes, check.HasLen, 1)
	c.Assert(nodes[0].Address(), check.Equals, srv.URL())
	c.Assert(nodes[0].Metadata(), check.DeepEquals, metadata)
	c.Assert(nodes[0].Pool(), check.DeepEquals, "p1")
	c.Assert(nodes[0].Status(), check.DeepEquals, "ready")
}

func (s *S) TestListNodesOnlyValid(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	metadata := map[string]string{"m1": "v1", "pool": "p1"}
	opts := provision.AddNodeOptions{
		Address:  srv.URL(),
		Metadata: metadata,
	}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	cli, err := docker.NewClient(srv.URL())
	c.Assert(err, check.IsNil)
	srv2, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	cli2, err := docker.NewClient(srv2.URL())
	c.Assert(err, check.IsNil)
	err = joinSwarm(cli, cli2, srv2.URL())
	c.Assert(err, check.IsNil)
	nodes, err := s.p.ListNodes(nil)
	c.Assert(err, check.IsNil)
	c.Assert(nodes, check.HasLen, 1)
	c.Assert(nodes[0].Address(), check.Equals, srv.URL())
	c.Assert(nodes[0].Metadata(), check.DeepEquals, metadata)
	c.Assert(nodes[0].Pool(), check.DeepEquals, "p1")
	c.Assert(nodes[0].Status(), check.DeepEquals, "ready")
	err = s.p.RemoveNode(provision.RemoveNodeOptions{
		Address: srv.URL(),
	})
	c.Assert(err, check.IsNil)
	nodes, err = s.p.ListNodes(nil)
	c.Assert(err, check.IsNil)
	c.Assert(nodes, check.HasLen, 0)
}

func (s *S) TestListNodesEmpty(c *check.C) {
	nodes, err := s.p.ListNodes(nil)
	c.Assert(err, check.IsNil)
	c.Assert(nodes, check.HasLen, 0)
}

func (s *S) TestRestart(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.Restart(a, "web", nil)
	c.Assert(err, check.IsNil)
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 1)
	err = s.p.Restart(a, "", nil)
	c.Assert(err, check.IsNil)
	units, err = s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 1)
}

func (s *S) TestRestartExisting(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 1, "web", nil)
	c.Assert(err, check.IsNil)
	err = s.p.Restart(a, "web", nil)
	c.Assert(err, check.IsNil)
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 1)
	cli, err := docker.NewClient(srv.URL())
	c.Assert(err, check.IsNil)
	service, err := cli.InspectService("myapp-web")
	c.Assert(err, check.IsNil)
	l := provision.LabelSet{Labels: service.Spec.TaskTemplate.ContainerSpec.Labels, Prefix: tsuruLabelPrefix}
	c.Assert(l.Restarts(), check.Equals, 1)
}

func (s *S) TestStopStart(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 3, "web", nil)
	c.Assert(err, check.IsNil)
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 3)
	err = s.p.Stop(a, "")
	c.Assert(err, check.IsNil)
	units, err = s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 0)
	err = s.p.Start(a, "")
	c.Assert(err, check.IsNil)
	units, err = s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 3)
}

func (s *S) TestStopStartSingleProcess(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web":    "python myapp.py",
			"worker": "python myworker.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 1, "web", nil)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 1, "worker", nil)
	c.Assert(err, check.IsNil)
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 2)
	err = s.p.Stop(a, "worker")
	c.Assert(err, check.IsNil)
	units, err = s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 1)
	c.Assert(units[0].ProcessName, check.Equals, "web")
	err = s.p.Start(a, "worker")
	c.Assert(err, check.IsNil)
	units, err = s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 2)
	procs := []string{units[0].ProcessName, units[1].ProcessName}
	sort.Strings(procs)
	c.Assert(procs, check.DeepEquals, []string{"web", "worker"})
}

func (s *S) TestUnits(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 1, "web", nil)
	c.Assert(err, check.IsNil)
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 1)
	expected := []provision.Unit{
		{ID: units[0].ID, Name: "", AppName: "myapp", ProcessName: "web", Type: "", Ip: "127.0.0.1", Status: "starting", Address: &url.URL{}},
	}
	c.Assert(units, check.DeepEquals, expected)
}

func (s *S) TestUnitsWithShutdownTasks(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 1, "web", nil)
	c.Assert(err, check.IsNil)
	cli, err := newClient(srv.URL())
	c.Assert(err, check.IsNil)
	tasks, err := cli.ListTasks(docker.ListTasksOptions{})
	c.Assert(err, check.IsNil)
	tasks[0].DesiredState = swarm.TaskStateShutdown
	err = srv.MutateTask(tasks[0].ID, tasks[0])
	c.Assert(err, check.IsNil)
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 0)
}

func (s *S) TestUnitsWithNoNodeIDServiceIDTasks(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 1, "web", nil)
	c.Assert(err, check.IsNil)
	cli, err := newClient(srv.URL())
	c.Assert(err, check.IsNil)
	tasks, err := cli.ListTasks(docker.ListTasksOptions{})
	c.Assert(err, check.IsNil)
	oldNodeID := tasks[0].NodeID
	tasks[0].NodeID = ""
	err = srv.MutateTask(tasks[0].ID, tasks[0])
	c.Assert(err, check.IsNil)
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 0)
	tasks[0].NodeID = oldNodeID
	tasks[0].ServiceID = ""
	err = srv.MutateTask(tasks[0].ID, tasks[0])
	c.Assert(err, check.IsNil)
	units, err = s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 0)
}

func (s *S) TestUnitsWithoutSwarmNode(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 1, "web", nil)
	c.Assert(err, check.IsNil)
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 1)
	err = s.p.RemoveNode(provision.RemoveNodeOptions{Address: srv.URL(), Rebalance: false})
	c.Assert(err, check.IsNil)
	units, err = s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 0)
}

func (s *S) TestRoutableUnits(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL(), Metadata: map[string]string{"pool": "px"}}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	err = provision.AddPool(provision.AddPoolOptions{Name: "px", Public: true, Provisioner: "swarm"})
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1, Pool: "px"}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 10, "web", nil)
	c.Assert(err, check.IsNil)
	addrs, err := s.p.RoutableAddresses(a)
	c.Assert(err, check.IsNil)
	c.Assert(addrs, check.DeepEquals, []url.URL{
		{Scheme: "http", Host: "127.0.0.1:30000"},
	})
}

func (s *S) TestRoutableUnitsNoNodesInPool(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL(), Metadata: map[string]string{"pool": "py"}}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	err = provision.AddPool(provision.AddPoolOptions{Name: "px", Public: true, Provisioner: "swarm"})
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1, Pool: "px"}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 10, "web", nil)
	c.Assert(err, check.IsNil)
	addrs, err := s.p.RoutableAddresses(a)
	c.Assert(err, check.IsNil)
	c.Assert(addrs, check.DeepEquals, []url.URL{})
}

func (s *S) TestAddUnits(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 3, "web", nil)
	c.Assert(err, check.IsNil)
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 3)
	err = s.p.AddUnits(a, 2, "", nil)
	c.Assert(err, check.IsNil)
	units, err = s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 5)
}

func (s *S) TestAddUnitsMultipleProcesses(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web":    "python myapp.py",
			"worker": "python myworker.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 1, "web", nil)
	c.Assert(err, check.IsNil)
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 1)
	err = s.p.AddUnits(a, 1, "worker", nil)
	c.Assert(err, check.IsNil)
	units, err = s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 2)
}

func (s *S) TestAddUnitsNoDeploys(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 3, "web", nil)
	c.Assert(err, check.ErrorMatches, `units can only be modified after the first deploy`)
}

func (s *S) TestAddUnitsNoProcessWithMultiple(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web":    "python myapp.py",
			"worker": "python myworker.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 3, "", nil)
	c.Assert(err, check.ErrorMatches, `process error: no process name specified and more than one declared in Procfile`)
}

func (s *S) TestAddUnitsNoImage(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 3, "web", nil)
	c.Assert(err, check.ErrorMatches, `no process information found deploying image "registry.tsuru.io/tsuru/app-myapp"`)
}

func (s *S) TestAddUnitsZeroUnits(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 0, "web", nil)
	c.Assert(err, check.ErrorMatches, `cannot change 0 units`)
}

func (s *S) TestAddUnitsWithHealthcheck(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
		"healthcheck": provision.TsuruYamlHealthcheck{
			Path: "/hc",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 1, "web", nil)
	c.Assert(err, check.IsNil)
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 1)
	cli, err := docker.NewClient(srv.URL())
	c.Assert(err, check.IsNil)
	service, err := cli.InspectService(serviceNameForApp(a, "web"))
	c.Assert(err, check.IsNil)
	c.Assert(service.Spec.TaskTemplate.ContainerSpec.Healthcheck, check.DeepEquals, &container.HealthConfig{
		Test: []string{
			"CMD-SHELL",
			"curl -XGET -fsSL http://localhost:8888/hc -o/dev/null -w '%{http_code}' | grep 200",
		},
		Timeout:  120 * time.Second,
		Retries:  1,
		Interval: 3 * time.Second,
	})
}

func (s *S) TestRemoveUnits(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 3, "web", nil)
	c.Assert(err, check.IsNil)
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 3)
	err = s.p.RemoveUnits(a, 2, "web", nil)
	c.Assert(err, check.IsNil)
	units, err = s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 1)
	err = s.p.RemoveUnits(a, 1, "web", nil)
	c.Assert(err, check.IsNil)
	units, err = s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 0)
	err = s.p.RemoveUnits(a, 1, "web", nil)
	c.Assert(err, check.ErrorMatches, `cannot have less than 0 units`)
}

func (s *S) TestGetNode(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	metadata := map[string]string{"m1": "v1", "pool": "p1"}
	opts := provision.AddNodeOptions{
		Address:  srv.URL(),
		Metadata: metadata,
	}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	node, err := s.p.GetNode(srv.URL())
	c.Assert(err, check.IsNil)
	c.Assert(node.Address(), check.Equals, srv.URL())
	c.Assert(node.Metadata(), check.DeepEquals, metadata)
	c.Assert(node.Pool(), check.DeepEquals, "p1")
	c.Assert(node.Status(), check.DeepEquals, "ready")
}

func (s *S) TestGetNodeNotFound(c *check.C) {
	_, err := s.p.GetNode("http://tai.shar.malkier")
	c.Assert(err, check.Equals, provision.ErrNodeNotFound)
}

func (s *S) TestRemoveNode(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	srv2, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv2.Stop()
	metadata := map[string]string{"m1": "v1", "pool": "p1"}
	opts := provision.AddNodeOptions{
		Address:  srv.URL(),
		Metadata: metadata,
	}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	opts = provision.AddNodeOptions{
		Address:  srv2.URL(),
		Metadata: metadata,
	}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	err = s.p.RemoveNode(provision.RemoveNodeOptions{
		Address: srv.URL(),
	})
	c.Assert(err, check.IsNil)
	_, err = s.p.GetNode(srv.URL())
	c.Assert(errors.Cause(err), check.Equals, provision.ErrNodeNotFound)
}

func (s *S) TestRemoveLastNodeLeaveSwarm(c *check.C) {
	left := false
	srv, err := testing.NewServer("127.0.0.1:0", nil, func(r *http.Request) {
		if r.URL.Path == "/swarm/leave" {
			left = true
		}
	})
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	metadata := map[string]string{"m1": "v1", "pool": "p1"}
	opts := provision.AddNodeOptions{
		Address:  srv.URL(),
		Metadata: metadata,
	}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	err = s.p.RemoveNode(provision.RemoveNodeOptions{
		Address: srv.URL(),
	})
	c.Assert(err, check.IsNil)
	_, err = s.p.GetNode(srv.URL())
	c.Assert(errors.Cause(err), check.Equals, provision.ErrNodeNotFound)
	c.Assert(left, check.Equals, true)
}

func (s *S) TestRemoveNodeRebalance(c *check.C) {
	var reqs []*http.Request
	srv, err := testing.NewServer("127.0.0.1:0", nil, func(r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/nodes/") {
			reqs = append(reqs, r)
		}
	})
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	srv2, err := testing.NewServer("127.0.0.1:0", nil, func(r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/nodes/") {
			reqs = append(reqs, r)
		}
	})
	c.Assert(err, check.IsNil)
	defer srv2.Stop()
	metadata := map[string]string{"m1": "v1", "pool": "p1"}
	opts := provision.AddNodeOptions{
		Address:  srv.URL(),
		Metadata: metadata,
	}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	opts = provision.AddNodeOptions{
		Address:  srv2.URL(),
		Metadata: metadata,
	}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	reqs = nil
	err = s.p.RemoveNode(provision.RemoveNodeOptions{
		Address:   srv.URL(),
		Rebalance: true,
	})
	c.Assert(err, check.IsNil)
	_, err = s.p.GetNode(srv.URL())
	c.Assert(errors.Cause(err), check.Equals, provision.ErrNodeNotFound)
	c.Assert(reqs, check.HasLen, 2)
	c.Assert(reqs[0].Method, check.Equals, "POST")
	c.Assert(reqs[1].Method, check.Equals, "DELETE")
}

func (s *S) TestRemoveNodeNotFound(c *check.C) {
	err := s.p.RemoveNode(provision.RemoveNodeOptions{
		Address: "localhost:1000",
	})
	c.Assert(errors.Cause(err), check.Equals, provision.ErrNodeNotFound)
}

func (s *S) TestUpdateNode(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	metadata := map[string]string{"m1": "v1", "pool": "p1"}
	opts := provision.AddNodeOptions{
		Address:  srv.URL(),
		Metadata: metadata,
	}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	err = s.p.UpdateNode(provision.UpdateNodeOptions{
		Address:  srv.URL(),
		Metadata: map[string]string{"m1": "v2", "m2": "v3"},
	})
	c.Assert(err, check.IsNil)
	node, err := s.p.GetNode(srv.URL())
	c.Assert(err, check.IsNil)
	c.Assert(node.Metadata(), check.DeepEquals, map[string]string{
		"m1":   "v2",
		"m2":   "v3",
		"pool": "p1",
	})
}

func (s *S) TestUpdateNodeDisableEnable(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	metadata := map[string]string{"m1": "v1", "pool": "p1"}
	opts := provision.AddNodeOptions{
		Address:  srv.URL(),
		Metadata: metadata,
	}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	err = s.p.UpdateNode(provision.UpdateNodeOptions{
		Address: srv.URL(),
		Disable: true,
	})
	c.Assert(err, check.IsNil)
	node, err := s.p.GetNode(srv.URL())
	c.Assert(err, check.IsNil)
	c.Assert(node.Metadata(), check.DeepEquals, map[string]string{
		"m1":   "v1",
		"pool": "p1",
	})
	c.Assert(node.Status(), check.Equals, "ready (pause)")
	err = s.p.UpdateNode(provision.UpdateNodeOptions{
		Address: srv.URL(),
		Enable:  true,
	})
	c.Assert(err, check.IsNil)
	node, err = s.p.GetNode(srv.URL())
	c.Assert(err, check.IsNil)
	c.Assert(node.Status(), check.Equals, "ready")
}

func (s *S) TestUpdateNodeNotFound(c *check.C) {
	err := s.p.UpdateNode(provision.UpdateNodeOptions{
		Address: "localhost:1000",
	})
	c.Assert(errors.Cause(err), check.Equals, provision.ErrNodeNotFound)
}

func (s *S) TestRegisterUnit(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", Platform: "whitespace", TeamOwner: s.team.Name}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	cli, err := chooseDBSwarmNode()
	c.Assert(err, check.IsNil)
	set, err := provision.ServiceLabels(provision.ServiceLabelsOpts{
		App: a,
		ServiceLabelExtendedOpts: provision.ServiceLabelExtendedOpts{
			IsDeploy:    true,
			BuildImage:  "app:v1",
			Provisioner: provisionerName,
			Prefix:      tsuruLabelPrefix,
		},
	})
	c.Assert(err, check.IsNil)
	_, err = cli.CreateService(docker.CreateServiceOptions{
		ServiceSpec: swarm.ServiceSpec{
			TaskTemplate: swarm.TaskSpec{
				ContainerSpec: swarm.ContainerSpec{
					Labels: set.ToLabels(),
				},
			},
			Annotations: swarm.Annotations{
				Name:   "myapp-web",
				Labels: set.ToLabels(),
			},
		},
	})
	c.Assert(err, check.IsNil)
	tasks, err := cli.ListTasks(docker.ListTasksOptions{})
	c.Assert(err, check.IsNil)
	c.Assert(tasks, check.HasLen, 1)
	var serviceBodies []string
	rollback := s.addServiceInstance(c, a.Name, nil, func(w http.ResponseWriter, r *http.Request) {
		data, _ := ioutil.ReadAll(r.Body)
		serviceBodies = append(serviceBodies, string(data))
		w.WriteHeader(http.StatusOK)
	})
	defer rollback()
	err = s.p.RegisterUnit(a, tasks[0].Status.ContainerStatus.ContainerID, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	data, err := image.GetImageCustomData("app:v1")
	c.Assert(err, check.IsNil)
	c.Assert(data.Processes, check.DeepEquals, map[string][]string{"web": {"python myapp.py"}})
	c.Assert(serviceBodies, check.HasLen, 1)
	c.Assert(serviceBodies[0], check.Matches, ".*unit-host=127.0.0.1")
}

func (s *S) TestRegisterUnitNotBuild(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", Platform: "whitespace", TeamOwner: s.team.Name}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	cli, err := chooseDBSwarmNode()
	c.Assert(err, check.IsNil)
	set, err := provision.ServiceLabels(provision.ServiceLabelsOpts{
		App: a,
		ServiceLabelExtendedOpts: provision.ServiceLabelExtendedOpts{
			BuildImage:  "notset:v1",
			Provisioner: provisionerName,
			Prefix:      tsuruLabelPrefix,
		},
	})
	c.Assert(err, check.IsNil)
	_, err = cli.CreateService(docker.CreateServiceOptions{
		ServiceSpec: swarm.ServiceSpec{
			TaskTemplate: swarm.TaskSpec{
				ContainerSpec: swarm.ContainerSpec{
					Labels: set.ToLabels(),
				},
			},
			Annotations: swarm.Annotations{
				Name:   "myapp-web",
				Labels: set.ToLabels(),
			},
		},
	})
	c.Assert(err, check.IsNil)
	conts, err := cli.ListContainers(docker.ListContainersOptions{})
	c.Assert(err, check.IsNil)
	c.Assert(conts, check.HasLen, 1)
	var serviceBodies []string
	rollback := s.addServiceInstance(c, a.Name, nil, func(w http.ResponseWriter, r *http.Request) {
		data, _ := ioutil.ReadAll(r.Body)
		serviceBodies = append(serviceBodies, string(data))
		w.WriteHeader(http.StatusOK)
	})
	defer rollback()
	err = s.p.RegisterUnit(a, conts[0].ID, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	data, err := image.GetImageCustomData("notset:v1")
	c.Assert(err, check.IsNil)
	c.Assert(data, check.DeepEquals, image.ImageMetadata{})
	c.Assert(serviceBodies, check.HasLen, 1)
	c.Assert(serviceBodies[0], check.Matches, ".*unit-host=127.0.0.1")
}

func (s *S) TestRegisterUnitNoImageLabel(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", Platform: "whitespace", TeamOwner: s.team.Name}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	cli, err := chooseDBSwarmNode()
	c.Assert(err, check.IsNil)
	set, err := provision.ServiceLabels(provision.ServiceLabelsOpts{
		App: a,
		ServiceLabelExtendedOpts: provision.ServiceLabelExtendedOpts{
			Provisioner: provisionerName,
			IsDeploy:    true,
			Prefix:      tsuruLabelPrefix,
		},
	})
	c.Assert(err, check.IsNil)
	_, err = cli.CreateService(docker.CreateServiceOptions{
		ServiceSpec: swarm.ServiceSpec{
			TaskTemplate: swarm.TaskSpec{
				ContainerSpec: swarm.ContainerSpec{
					Labels: set.ToLabels(),
				},
			},
			Annotations: swarm.Annotations{
				Name:   "myapp-web",
				Labels: set.ToLabels(),
			},
		},
	})
	c.Assert(err, check.IsNil)
	tasks, err := cli.ListTasks(docker.ListTasksOptions{})
	c.Assert(err, check.IsNil)
	c.Assert(tasks, check.HasLen, 1)
	err = s.p.RegisterUnit(a, tasks[0].Status.ContainerStatus.ContainerID, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.ErrorMatches, `invalid build image label for build task: .*`)
}

func (s *S) TestUploadDeploy(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", Platform: "whitespace", TeamOwner: s.team.Name}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	attached := s.attachRegister(c, srv, true, a)
	evt, err := event.New(&event.Opts{
		Target:  event.Target{Type: event.TargetTypeApp, Value: a.GetName()},
		Kind:    permission.PermAppDeploy,
		Owner:   s.token,
		Allowed: event.Allowed(permission.PermAppDeploy),
	})
	c.Assert(err, check.IsNil)
	buf := strings.NewReader("my upload data")
	imgID, err := s.p.UploadDeploy(a, ioutil.NopCloser(buf), int64(buf.Len()), false, evt)
	c.Assert(err, check.IsNil)
	c.Assert(<-attached, check.Equals, true)
	c.Assert(imgID, check.Equals, "registry.tsuru.io/tsuru/app-myapp:v1")
	dbImg, err := image.AppCurrentImageName(a.GetName())
	c.Assert(err, check.IsNil)
	c.Assert(dbImg, check.Equals, "registry.tsuru.io/tsuru/app-myapp:v1")
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 1)
	c.Assert(units, check.DeepEquals, []provision.Unit{
		{ID: units[0].ID, AppName: a.Name, Type: "whitespace", ProcessName: "web", Ip: "127.0.0.1", Status: "starting", Address: &url.URL{}},
	})
	cli, err := docker.NewClient(srv.URL())
	c.Assert(err, check.IsNil)
	task, err := cli.InspectTask(units[0].ID)
	c.Assert(err, check.IsNil)
	cont, err := cli.InspectContainer(task.Status.ContainerStatus.ContainerID)
	c.Assert(err, check.IsNil)
	c.Assert(cont.Config.Entrypoint, check.DeepEquals, []string{
		"/bin/sh",
		"-lc",
		fmt.Sprintf(
			"[ -d /home/application/current ] && cd /home/application/current; %s && exec python myapp.py",
			extraRegisterCmds(a),
		),
	})
}

func (s *S) TestArchiveDeploy(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", Platform: "whitespace", TeamOwner: s.team.Name}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	attached := s.attachRegister(c, srv, true, a)
	evt, err := event.New(&event.Opts{
		Target:  event.Target{Type: event.TargetTypeApp, Value: a.GetName()},
		Kind:    permission.PermAppDeploy,
		Owner:   s.token,
		Allowed: event.Allowed(permission.PermAppDeploy),
	})
	c.Assert(err, check.IsNil)
	imgID, err := s.p.ArchiveDeploy(a, "http://server/myfile.tgz", evt)
	c.Assert(err, check.IsNil)
	c.Assert(<-attached, check.Equals, true)
	c.Assert(imgID, check.Equals, "registry.tsuru.io/tsuru/app-myapp:v1")
	dbImg, err := image.AppCurrentImageName(a.GetName())
	c.Assert(err, check.IsNil)
	c.Assert(dbImg, check.Equals, "registry.tsuru.io/tsuru/app-myapp:v1")
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 1)
	c.Assert(units, check.DeepEquals, []provision.Unit{
		{ID: units[0].ID, AppName: a.Name, Type: "whitespace", ProcessName: "web", Ip: "127.0.0.1", Status: "starting", Address: &url.URL{}},
	})
	cli, err := docker.NewClient(srv.URL())
	c.Assert(err, check.IsNil)
	task, err := cli.InspectTask(units[0].ID)
	c.Assert(err, check.IsNil)
	cont, err := cli.InspectContainer(task.Status.ContainerStatus.ContainerID)
	c.Assert(err, check.IsNil)
	c.Assert(cont.Config.Entrypoint, check.DeepEquals, []string{
		"/bin/sh",
		"-lc",
		fmt.Sprintf(
			"[ -d /home/application/current ] && cd /home/application/current; %s && exec python myapp.py",
			extraRegisterCmds(a),
		),
	})
}

func (s *S) TestDeployServiceBind(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", Platform: "whitespace", TeamOwner: s.team.Name}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	attached := s.attachRegister(c, srv, true, a)
	var serviceBodies []string
	rollback := s.addServiceInstance(c, a.Name, nil, func(w http.ResponseWriter, r *http.Request) {
		data, _ := ioutil.ReadAll(r.Body)
		serviceBodies = append(serviceBodies, string(data))
		w.WriteHeader(http.StatusOK)
	})
	defer rollback()
	evt, err := event.New(&event.Opts{
		Target:  event.Target{Type: event.TargetTypeApp, Value: a.GetName()},
		Kind:    permission.PermAppDeploy,
		Owner:   s.token,
		Allowed: event.Allowed(permission.PermAppDeploy),
	})
	c.Assert(err, check.IsNil)
	imgID, err := s.p.ArchiveDeploy(a, "http://server/myfile.tgz", evt)
	c.Assert(err, check.IsNil)
	c.Assert(<-attached, check.Equals, true)
	c.Assert(imgID, check.Equals, "registry.tsuru.io/tsuru/app-myapp:v1")
	dbImg, err := image.AppCurrentImageName(a.GetName())
	c.Assert(err, check.IsNil)
	c.Assert(dbImg, check.Equals, "registry.tsuru.io/tsuru/app-myapp:v1")
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 1)
	c.Assert(units, check.DeepEquals, []provision.Unit{
		{ID: units[0].ID, AppName: a.Name, Type: "whitespace", ProcessName: "web", Ip: "127.0.0.1", Status: "starting", Address: &url.URL{}},
	})
	cli, err := docker.NewClient(srv.URL())
	c.Assert(err, check.IsNil)
	task, err := cli.InspectTask(units[0].ID)
	c.Assert(err, check.IsNil)
	c.Assert(task.Spec.ContainerSpec.Command, check.DeepEquals, []string{
		"/bin/sh",
		"-lc",
		fmt.Sprintf(
			"[ -d /home/application/current ] && cd /home/application/current; %s && exec python myapp.py",
			extraRegisterCmds(a),
		),
	})
	c.Assert(serviceBodies, check.HasLen, 1)
	c.Assert(serviceBodies[0], check.Matches, ".*unit-host="+units[0].Ip)
}

func (s *S) TestImageDeploy(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	imageName := "myimg:v1"
	srv.CustomHandler(fmt.Sprintf("/images/%s/json", imageName), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := docker.Image{
			Config: &docker.Config{
				Entrypoint:   []string{"/bin/sh", "-c", "python test.py"},
				ExposedPorts: map[docker.Port]struct{}{"80/tcp": {}},
			},
		}
		j, _ := json.Marshal(response)
		w.Write(j)
	}))
	cli, err := docker.NewClient(srv.URL())
	c.Assert(err, check.IsNil)
	err = cli.PullImage(docker.PullImageOptions{
		Repository: "myimg",
		Tag:        "v1",
	}, docker.AuthConfiguration{})
	c.Assert(err, check.IsNil)
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	attached := s.attachRegister(c, srv, false, a)
	evt, err := event.New(&event.Opts{
		Target:  event.Target{Type: event.TargetTypeApp, Value: a.GetName()},
		Kind:    permission.PermAppDeploy,
		Owner:   s.token,
		Allowed: event.Allowed(permission.PermAppDeploy),
	})
	c.Assert(err, check.IsNil)
	deployedImg, err := s.p.ImageDeploy(a, imageName, evt)
	c.Assert(err, check.IsNil)
	c.Assert(<-attached, check.Equals, true)
	c.Assert(deployedImg, check.Equals, "registry.tsuru.io/tsuru/app-myapp:v1")
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 1)
	c.Assert(units, check.DeepEquals, []provision.Unit{
		{ID: units[0].ID, AppName: a.Name, ProcessName: "web", Ip: "127.0.0.1", Status: "starting", Address: &url.URL{}},
	})
	dbImg, err := image.AppCurrentImageName(a.GetName())
	c.Assert(err, check.IsNil)
	c.Assert(dbImg, check.Equals, "registry.tsuru.io/tsuru/app-myapp:v1")
	task, err := cli.InspectTask(units[0].ID)
	c.Assert(err, check.IsNil)
	cont, err := cli.InspectContainer(task.Status.ContainerStatus.ContainerID)
	c.Assert(err, check.IsNil)
	c.Assert(cont.Config.Entrypoint, check.DeepEquals, []string{
		"/bin/sh",
		"-lc",
		fmt.Sprintf(
			"[ -d /home/application/current ] && cd /home/application/current; %s && exec $0 \"$@\"",
			extraRegisterCmds(a),
		),
		"/bin/sh", "-c", "python test.py",
	})
}

func (s *S) TestDestroy(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 1, "web", nil)
	c.Assert(err, check.IsNil)
	err = s.p.Destroy(a)
	c.Assert(err, check.IsNil)
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 0)
}

func (s *S) TestDestroyServiceNotFound(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	err = s.p.Destroy(a)
	c.Assert(err, check.IsNil)
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 0)
}

func (s *S) TestShellToAnAppByAppName(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	err = s.p.AddNode(provision.AddNodeOptions{Address: srv.URL()})
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 1, "web", nil)
	c.Assert(err, check.IsNil)
	buf := safe.NewBuffer([]byte("echo test"))
	conn := &provisiontest.FakeConn{Buf: buf}
	opts := provision.ShellOptions{App: a, Conn: conn, Width: 140, Height: 38, Term: "xterm"}
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	client, _ := docker.NewClient(srv.URL())
	task, err := client.InspectTask(units[0].ID)
	c.Assert(err, check.IsNil)
	task.DesiredState = swarm.TaskStateRunning
	err = srv.MutateTask(task.ID, *task)
	c.Assert(err, check.IsNil)
	var urls struct {
		items []url.URL
		sync.Mutex
	}
	srv.PrepareExec("*", func() {
		time.Sleep(500e6)
	})
	srv.SetHook(func(r *http.Request) {
		urls.Lock()
		urls.items = append(urls.items, *r.URL)
		urls.Unlock()
	})
	err = s.p.Shell(opts)
	c.Assert(err, check.IsNil)
	urls.Lock()
	resizeURL := urls.items[len(urls.items)-2]
	urls.Unlock()
	execResizeRegexp := regexp.MustCompile(`^.*/exec/(.*)/resize$`)
	matches := execResizeRegexp.FindStringSubmatch(resizeURL.Path)
	c.Assert(matches, check.HasLen, 2)
	c.Assert(resizeURL.Query().Get("w"), check.Equals, "140")
	c.Assert(resizeURL.Query().Get("h"), check.Equals, "38")
	exec, err := client.InspectExec(matches[1])
	c.Assert(err, check.IsNil)
	cmd := append([]string{exec.ProcessConfig.EntryPoint}, exec.ProcessConfig.Arguments...)
	c.Assert(cmd, check.DeepEquals, []string{"/usr/bin/env", "TERM=xterm", "bash", "-l"})
}

func (s *S) TestShellToAnAppByTaskID(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	err = s.p.AddNode(provision.AddNodeOptions{Address: srv.URL()})
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 2, "web", nil)
	c.Assert(err, check.IsNil)
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	buf := safe.NewBuffer([]byte("echo test"))
	conn := &provisiontest.FakeConn{Buf: buf}
	opts := provision.ShellOptions{App: a, Conn: conn, Width: 140, Height: 38, Unit: units[1].ID, Term: "xterm"}
	client, _ := docker.NewClient(srv.URL())
	task, err := client.InspectTask(units[1].ID)
	c.Assert(err, check.IsNil)
	task.DesiredState = swarm.TaskStateRunning
	err = srv.MutateTask(task.ID, *task)
	c.Assert(err, check.IsNil)
	var urls struct {
		items []url.URL
		sync.Mutex
	}
	srv.PrepareExec("*", func() {
		time.Sleep(500e6)
	})
	srv.SetHook(func(r *http.Request) {
		urls.Lock()
		urls.items = append(urls.items, *r.URL)
		urls.Unlock()
	})
	err = s.p.Shell(opts)
	c.Assert(err, check.IsNil)
	urls.Lock()
	resizeURL := urls.items[len(urls.items)-2]
	urls.Unlock()
	execResizeRegexp := regexp.MustCompile(`^.*/exec/(.*)/resize$`)
	matches := execResizeRegexp.FindStringSubmatch(resizeURL.Path)
	c.Assert(matches, check.HasLen, 2)
	c.Assert(resizeURL.Query().Get("w"), check.Equals, "140")
	c.Assert(resizeURL.Query().Get("h"), check.Equals, "38")
	exec, err := client.InspectExec(matches[1])
	c.Assert(err, check.IsNil)
	c.Assert(exec.ContainerID, check.Equals, task.Status.ContainerStatus.ContainerID)
	cmd := append([]string{exec.ProcessConfig.EntryPoint}, exec.ProcessConfig.Arguments...)
	c.Assert(cmd, check.DeepEquals, []string{"/usr/bin/env", "TERM=xterm", "bash", "-l"})
}

func (s *S) TestExecuteCommand(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	err = s.p.AddNode(provision.AddNodeOptions{Address: srv.URL()})
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 3, "web", nil)
	c.Assert(err, check.IsNil)
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	client, _ := docker.NewClient(srv.URL())
	task, err := client.InspectTask(units[0].ID)
	c.Assert(err, check.IsNil)
	task.DesiredState = swarm.TaskStateRunning
	err = srv.MutateTask(task.ID, *task)
	c.Assert(err, check.IsNil)
	task, err = client.InspectTask(units[2].ID)
	c.Assert(err, check.IsNil)
	task.DesiredState = swarm.TaskStateRunning
	err = srv.MutateTask(task.ID, *task)
	c.Assert(err, check.IsNil)
	var executed int
	srv.SetHook(func(r *http.Request) {
		srv.PrepareExec("*", func() {
			executed++
		})
	})
	var stdout, stderr bytes.Buffer
	err = s.p.ExecuteCommand(&stdout, &stderr, a, "ls", "-l")
	c.Assert(err, check.IsNil)
	c.Assert(executed, check.Equals, 2)
}

func (s *S) TestExecuteCommandNoRunningTask(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	err = s.p.AddNode(provision.AddNodeOptions{Address: srv.URL()})
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 3, "web", nil)
	c.Assert(err, check.IsNil)
	var stdout, stderr bytes.Buffer
	err = s.p.ExecuteCommand(&stdout, &stderr, a, "ls", "-l")
	c.Assert(err, check.DeepEquals, provision.ErrEmptyApp)
}

func (s *S) TestExecuteCommandOnce(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	err = s.p.AddNode(provision.AddNodeOptions{Address: srv.URL()})
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 3, "web", nil)
	c.Assert(err, check.IsNil)
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	client, _ := docker.NewClient(srv.URL())
	task, err := client.InspectTask(units[0].ID)
	c.Assert(err, check.IsNil)
	task.DesiredState = swarm.TaskStateRunning
	err = srv.MutateTask(task.ID, *task)
	c.Assert(err, check.IsNil)
	task, err = client.InspectTask(units[2].ID)
	c.Assert(err, check.IsNil)
	task.DesiredState = swarm.TaskStateRunning
	err = srv.MutateTask(task.ID, *task)
	c.Assert(err, check.IsNil)
	var executed int
	srv.SetHook(func(r *http.Request) {
		srv.PrepareExec("*", func() {
			executed++
		})
	})
	var stdout, stderr bytes.Buffer
	err = s.p.ExecuteCommandOnce(&stdout, &stderr, a, "ls", "-l")
	c.Assert(err, check.IsNil)
	c.Assert(executed, check.Equals, 1)
}

func (s *S) TestExecuteCommandOnceNoRunningTask(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	err = s.p.AddNode(provision.AddNodeOptions{Address: srv.URL()})
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 3, "web", nil)
	c.Assert(err, check.IsNil)
	var stdout, stderr bytes.Buffer
	err = s.p.ExecuteCommandOnce(&stdout, &stderr, a, "ls", "-l")
	c.Assert(err, check.DeepEquals, provision.ErrEmptyApp)
}

func (s *S) TestExecuteCommandIsolated(c *check.C) {
	containerChan := make(chan *docker.Container, 1)
	srv, err := testing.NewServer("127.0.0.1:0", containerChan, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	err = s.p.AddNode(provision.AddNodeOptions{Address: srv.URL()})
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	attached := s.attachRegister(c, srv, false, a)
	var stdout, stderr bytes.Buffer
	var service *swarm.Service
	client, _ := docker.NewClient(srv.URL())
	srv.CustomHandler("/services/create", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.DefaultHandler().ServeHTTP(w, r)
		service, err = client.InspectService("myapp-isolated-run")
		c.Assert(err, check.IsNil)
	}))
	err = s.p.ExecuteCommandIsolated(&stdout, &stderr, a, "ls", "-l")
	c.Assert(err, check.IsNil)
	c.Assert(<-attached, check.Equals, true)
	cont := <-containerChan
	c.Assert(cont.Image, check.Equals, "myapp:v1")
	_, err = client.InspectService("myapp-isolated-run")
	c.Assert(err, check.DeepEquals, &docker.NoSuchService{ID: "myapp-isolated-run"})
	l := provision.LabelSet{Labels: service.Spec.Labels, Prefix: tsuruLabelPrefix}
	c.Assert(l.IsIsolatedRun(), check.Equals, true)
}

func (s *S) TestExecuteCommandIsolatedNoDeploys(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	err = s.p.AddNode(provision.AddNodeOptions{Address: srv.URL()})
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp-2", TeamOwner: s.team.Name}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	var stdout, stderr bytes.Buffer
	err = s.p.ExecuteCommandIsolated(&stdout, &stderr, a, "ls", "-l")
	c.Assert(err, check.ErrorMatches, "*deploy*")
}

func (s *S) TestUpgradeNodeContainerCreatesBaseService(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	client, err := docker.NewClient(srv.URL())
	c.Assert(err, check.IsNil)
	err = s.p.AddNode(provision.AddNodeOptions{Address: srv.URL()})
	c.Assert(err, check.IsNil)
	c1 := nodecontainer.NodeContainerConfig{
		Name: "bs",
		Config: docker.Config{
			Image: "bsimg",
		},
	}
	err = nodecontainer.AddNewContainer("", &c1)
	c.Assert(err, check.IsNil)
	err = s.p.UpgradeNodeContainer("bs", "", ioutil.Discard)
	c.Assert(err, check.IsNil)
	service, err := client.InspectService("node-container-bs-all")
	c.Assert(err, check.IsNil)
	c.Assert(service.Spec.TaskTemplate.Placement.Constraints, check.DeepEquals, []string(nil))
}

func (s *S) TestUpgradeNodeContainerCreatesLimitedService(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	client, err := docker.NewClient(srv.URL())
	c.Assert(err, check.IsNil)
	err = s.p.AddNode(provision.AddNodeOptions{Address: srv.URL()})
	c.Assert(err, check.IsNil)
	c1 := nodecontainer.NodeContainerConfig{
		Name: "bs",
		Config: docker.Config{
			Image: "bsimg",
		},
	}
	err = nodecontainer.AddNewContainer("", &c1)
	c.Assert(err, check.IsNil)
	err = s.p.UpgradeNodeContainer("bs", "", ioutil.Discard)
	c.Assert(err, check.IsNil)
	err = nodecontainer.AddNewContainer("p1", &c1)
	c.Assert(err, check.IsNil)
	err = s.p.UpgradeNodeContainer("bs", "p1", ioutil.Discard)
	c.Assert(err, check.IsNil)
	err = s.p.UpgradeNodeContainer("bs", "p1", ioutil.Discard)
	c.Assert(err, check.IsNil)
	service, err := client.InspectService("node-container-bs-all")
	c.Assert(err, check.IsNil)
	c.Assert(service.Spec.TaskTemplate.Placement.Constraints, check.DeepEquals, []string{"node.labels.pool != p1"})
	service, err = client.InspectService("node-container-bs-p1")
	c.Assert(err, check.IsNil)
	c.Assert(service.Spec.TaskTemplate.Placement.Constraints, check.DeepEquals, []string{"node.labels.pool == p1"})
}

func (s *S) TestUpgradeNodeContainerBaseUpgradesSpecifics(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	client, err := docker.NewClient(srv.URL())
	c.Assert(err, check.IsNil)
	err = s.p.AddNode(provision.AddNodeOptions{Address: srv.URL()})
	c.Assert(err, check.IsNil)
	c1 := nodecontainer.NodeContainerConfig{
		Name: "bs",
		Config: docker.Config{
			Image: "bsimg",
		},
	}
	err = nodecontainer.AddNewContainer("", &c1)
	c.Assert(err, check.IsNil)
	err = nodecontainer.AddNewContainer("p1", &c1)
	c.Assert(err, check.IsNil)
	err = s.p.UpgradeNodeContainer("bs", "", ioutil.Discard)
	c.Assert(err, check.IsNil)
	service, err := client.InspectService("node-container-bs-all")
	c.Assert(err, check.IsNil)
	c.Assert(service.Spec.TaskTemplate.Placement.Constraints, check.DeepEquals, []string{"node.labels.pool != p1"})
	service, err = client.InspectService("node-container-bs-p1")
	c.Assert(err, check.IsNil)
	c.Assert(service.Spec.TaskTemplate.Placement.Constraints, check.DeepEquals, []string{"node.labels.pool == p1"})
}

func (s *S) TestUpgradeNodeContainerUpdatesExistingService(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	client, err := docker.NewClient(srv.URL())
	c.Assert(err, check.IsNil)
	err = s.p.AddNode(provision.AddNodeOptions{Address: srv.URL()})
	c.Assert(err, check.IsNil)
	c1 := nodecontainer.NodeContainerConfig{
		Name: "bs",
		Config: docker.Config{
			Image: "bsimg",
		},
	}
	err = nodecontainer.AddNewContainer("", &c1)
	c.Assert(err, check.IsNil)
	err = s.p.UpgradeNodeContainer("bs", "", ioutil.Discard)
	c.Assert(err, check.IsNil)
	c1.Config.Image = "bs:v2"
	err = nodecontainer.UpdateContainer("", &c1)
	c.Assert(err, check.IsNil)
	err = s.p.UpgradeNodeContainer("bs", "", ioutil.Discard)
	c.Assert(err, check.IsNil)
	service, err := client.InspectService("node-container-bs-all")
	c.Assert(err, check.IsNil)
	c.Assert(service.Spec.TaskTemplate.ContainerSpec.Image, check.Equals, "bs:v2")
}

func (s *S) TestRemoveNodeContainerRemovesService(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	client, err := docker.NewClient(srv.URL())
	c.Assert(err, check.IsNil)
	err = s.p.AddNode(provision.AddNodeOptions{Address: srv.URL()})
	c.Assert(err, check.IsNil)
	c1 := nodecontainer.NodeContainerConfig{
		Name: "bs",
		Config: docker.Config{
			Image: "bsimg",
		},
	}
	err = nodecontainer.AddNewContainer("", &c1)
	c.Assert(err, check.IsNil)
	err = s.p.UpgradeNodeContainer("bs", "", ioutil.Discard)
	c.Assert(err, check.IsNil)
	services, err := client.ListServices(docker.ListServicesOptions{})
	c.Assert(err, check.IsNil)
	c.Assert(len(services), check.Equals, 1)
	err = s.p.RemoveNodeContainer("bs", "", ioutil.Discard)
	c.Assert(err, check.IsNil)
	services, err = client.ListServices(docker.ListServicesOptions{})
	c.Assert(err, check.IsNil)
	c.Assert(len(services), check.Equals, 0)
}

func (s *S) TestNodeForNodeData(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 1, "web", nil)
	c.Assert(err, check.IsNil)
	cli, err := newClient(srv.URL())
	c.Assert(err, check.IsNil)
	conts, err := cli.ListContainers(docker.ListContainersOptions{})
	c.Assert(err, check.IsNil)
	c.Assert(conts, check.HasLen, 1)
	data := provision.NodeStatusData{
		Units: []provision.UnitStatusData{
			{ID: conts[0].ID},
		},
	}
	node, err := s.p.NodeForNodeData(data)
	c.Assert(err, check.IsNil)
	c.Assert(node.Address(), check.Equals, srv.URL())
	data = provision.NodeStatusData{
		Units: []provision.UnitStatusData{
			{ID: "invalidid"},
		},
	}
	_, err = s.p.NodeForNodeData(data)
	c.Assert(err, check.Equals, provision.ErrNodeNotFound)
}

func (s *S) attachRegister(c *check.C, srv *testing.DockerServer, register bool, a provision.App) <-chan bool {
	chAttached := make(chan bool, 1)
	srv.CustomHandler("/containers", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) == 4 && parts[3] == "attach" {
			if register {
				err := s.p.RegisterUnit(a, parts[2], map[string]interface{}{
					"processes": map[string]interface{}{
						"web": "python myapp.py",
					},
				})
				c.Assert(err, check.IsNil)
			}
			srv.MutateContainer(parts[2], docker.State{StartedAt: time.Now(), Running: false})
			chAttached <- true
		}
		srv.DefaultHandler().ServeHTTP(w, r)
	}))
	return chAttached
}

func (s *S) TestSleepStart(c *check.C) {
	srv, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer srv.Stop()
	opts := provision.AddNodeOptions{Address: srv.URL()}
	err = s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name, Deploys: 1}
	err = app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	imgName := "myapp:v1"
	err = image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 3, "web", nil)
	c.Assert(err, check.IsNil)
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 3)
	err = s.p.Sleep(a, "")
	c.Assert(err, check.IsNil)
	units, err = s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 0)
	err = s.p.Start(a, "")
	c.Assert(err, check.IsNil)
	units, err = s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 3)
}
