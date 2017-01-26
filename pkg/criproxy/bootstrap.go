/*
Copyright 2016 Mirantis

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

package criproxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"time"

	// TODO: switch to https://github.com/docker/docker/tree/master/client
	// Docker version used in k8s is too old for it
	dockermessage "github.com/docker/docker/pkg/jsonmessage"
	dockerclient "github.com/docker/engine-api/client"
	dockertypes "github.com/docker/engine-api/types"
	dockercontainer "github.com/docker/engine-api/types/container"
	dockerfilters "github.com/docker/engine-api/types/filters"

	// FIXME: use client-go
	// In particular, see https://github.com/kubernetes/client-go/blob/master/examples/in-cluster/main.go
	"k8s.io/kubernetes/pkg/api"
	cfg "k8s.io/kubernetes/pkg/apis/componentconfig/v1alpha1"
	clientset "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	"k8s.io/kubernetes/pkg/client/restclient"
)

const (
	// TODO: use the same constant/setting in different parts of code
	proxyRuntimeEndpoint    = "/run/criproxy.sock"
	internalDockerEndpoint  = "/var/run/docker.sock"
	busyboxImageName        = "busybox:1.26.2"
	proxyStopTimeoutSeconds = 5
	confFileMode            = 0600
)

var kubeletSettingsForCriProxy map[string]interface{} = map[string]interface{}{
	"containerRuntime":      "remote",
	"enableCRI":             true,
	"remoteRuntimeEndpoint": proxyRuntimeEndpoint,
	"remoteImageEndpoint":   proxyRuntimeEndpoint,
}

func loadJson(baseUrl, suffix string) (map[string]interface{}, error) {
	url := strings.TrimSuffix(baseUrl, "/") + suffix
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: tr}
	res, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("trying to get %q: %v", url, err)
	}

	defer res.Body.Close()
	var r map[string]interface{}
	d := json.NewDecoder(res.Body)
	d.UseNumber() // avoid getting floats
	if err := d.Decode(&r); err != nil {
		return nil, fmt.Errorf("failed to unmarshal json from %q: %v", url, err)
	}
	return r, nil
}

func writeJson(data interface{}, path string) error {
	bs, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal json: %v", err)
	}
	if err := ioutil.WriteFile(path, bs, confFileMode); err != nil {
		return fmt.Errorf("error writing %q: %v", path, err)
	}
	return nil
}

func getKubeletConfig(configzBaseUrl string) (map[string]interface{}, error) {
	cfg, err := loadJson(configzBaseUrl, "/configz")
	if err != nil {
		return nil, err
	}
	kubeletCfg, ok := cfg["componentconfig"].(map[string]interface{})
	if !ok {
		return nil, errors.New("couldn't get componentconfig from /configz")
	}
	return kubeletCfg, nil
}

func kubeletUpdated(kubeletCfg map[string]interface{}) bool {
	for k, v := range kubeletSettingsForCriProxy {
		if kubeletCfg[k] != v {
			return false
		}
	}
	return true
}
func updateKubeletConfig(kubeletCfg map[string]interface{}) {
	for k, v := range kubeletSettingsForCriProxy {
		kubeletCfg[k] = v
	}
}

func getNodeNameFromKubelet(statsBaseUrl string) (string, error) {
	stats, err := loadJson(statsBaseUrl, "/stats/summary")
	if err != nil {
		return "", err
	}
	nodeProps, ok := stats["node"].(map[string]interface{})
	if !ok {
		return "", errors.New("couldn't get node properties /stats/summary")
	}
	nodeName, _ := nodeProps["nodeName"].(string)
	if nodeName == "" {
		return "", errors.New("couldn't get node name via /stats/summary")
	}
	return nodeName, nil
}

func buildConfigMap(nodeName string, kubeletCfg map[string]interface{}) *api.ConfigMap {
	text, err := json.Marshal(kubeletCfg)
	if err != nil {
		log.Panicf("couldn't marshal kubelet config: %v", err)
	}
	return &api.ConfigMap{
		ObjectMeta: api.ObjectMeta{
			Name:      "kubelet-" + nodeName,
			Namespace: "kube-system",
		},
		Data: map[string]string{
			"kubelet.config": string(text),
		},
	}
}

func putConfigMap(cs clientset.Interface, configMap *api.ConfigMap) error {
	_, err := cs.Core().ConfigMaps("kube-system").Create(configMap)
	return err
}

func patchKubeletConfig(configzBaseUrl, statsBaseUrl, savedConfigPath string, cs clientset.Interface) (patched bool, dockerEndpoint string, err error) {
	kubeletCfg, err := getKubeletConfig(configzBaseUrl)
	if err != nil {
		return false, "", err
	}
	if kubeletUpdated(kubeletCfg) {
		return false, "", nil
	}
	if err := writeJson(kubeletCfg, savedConfigPath); err != nil {
		return false, "", err
	}
	updateKubeletConfig(kubeletCfg)

	dockerEp, ok := kubeletCfg["dockerEndpoint"].(string)
	if !ok {
		return false, "", errors.New("failed to retrieve docker endpoint from kubelet config")
	}

	nodeName, err := getNodeNameFromKubelet(statsBaseUrl)
	if err != nil {
		return false, "", err
	}
	if err := putConfigMap(cs, buildConfigMap(nodeName, kubeletCfg)); err != nil {
		return false, "", fmt.Errorf("failed to put ConfigMap: %v", err)
	}
	return true, dockerEp, nil
}

func pullImage(ctx context.Context, client *dockerclient.Client, imageName string, print bool) error {
	resp, err := client.ImagePull(ctx, imageName, dockertypes.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("Failed to pull busybox image: %v", err)
	}

	decoder := json.NewDecoder(resp)
	for {
		var msg dockermessage.JSONMessage
		err := decoder.Decode(&msg)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("error decoding docker message: %v", err)
		}
		if msg.Error != nil {
			return msg.Error
		}
		if print {
			fmt.Println(msg.Status)
		}
	}
	return nil
}

func installCriProxyContainer(dockerEndpoint, endpointToPass, proxyPath string, args []string) (string, error) {
	ctx := context.Background()

	client, err := dockerclient.NewClient(dockerEndpoint, "", nil, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create Docker client: %v", err)
	}

	filterArgs := dockerfilters.NewArgs()
	filterArgs.Add("label", "criproxy")
	containers, err := client.ContainerList(ctx, dockertypes.ContainerListOptions{
		Filter: filterArgs,
	})
	if len(containers) > 0 {
		for _, container := range containers {
			if err := client.ContainerRemove(ctx, container.ID, dockertypes.ContainerRemoveOptions{
				Force: true,
			}); err != nil {
				return "", fmt.Errorf("failed to remove old container: %v", err)
			}
		}
	}

	if err := pullImage(ctx, client, busyboxImageName, true); err != nil {
		return "", fmt.Errorf("failed to pull busybox image: %v", err)
	}

	binds := []string{
		"/run:/run",
		fmt.Sprintf("%s:%s", proxyPath, "/criproxy"),
	}
	if strings.HasPrefix(endpointToPass, "unix://") {
		// mount the endpoint as a fixed path into the proxy container
		socketPath := strings.TrimPrefix(endpointToPass, "unix://")
		binds = append(binds, fmt.Sprintf("%s:%s", socketPath, internalDockerEndpoint))
		endpointToPass = "unix://" + internalDockerEndpoint
	}
	containerName := fmt.Sprintf("criproxy-%d", time.Now().UnixNano())
	resp, err := client.ContainerCreate(ctx, &dockercontainer.Config{
		Image:  busyboxImageName,
		Labels: map[string]string{"criproxy": "true"},
		Env:    []string{"DOCKER_HOST=" + endpointToPass},
		Cmd:    append([]string{"/criproxy"}, args...),
	}, &dockercontainer.HostConfig{
		// the proxy should be able to connect to a docker endpoint on localhost, too
		NetworkMode: "host",
		Binds:       binds,
		RestartPolicy: dockercontainer.RestartPolicy{
			Name: "always",
		},
	}, nil, containerName)
	if err != nil {
		return "", fmt.Errorf("failed to create CRI proxy container: %v", err)
	}
	if err := client.ContainerStart(ctx, resp.ID); err != nil {
		client.ContainerRemove(ctx, resp.ID, dockertypes.ContainerRemoveOptions{
			Force: true,
		})
		return "", fmt.Errorf("failed to start CRI proxy container: %v", err)
	}
	return resp.ID, nil
}

type BootstrapConfig struct {
	ConfigzBaseUrl  string
	StatsBaseUrl    string
	SavedConfigPath string
	ProxyPath       string
	ProxyArgs       []string
	ProxySocketPath string
}

func EnsureCRIProxy(bootConfig *BootstrapConfig) (bool, error) {
	if bootConfig.ConfigzBaseUrl == "" || bootConfig.StatsBaseUrl == "" || bootConfig.ProxyPath == "" || bootConfig.ProxySocketPath == "" {
		return false, errors.New("invalid BootstrapConfig")
	}
	config, err := restclient.InClusterConfig()
	if err != nil {
		return false, fmt.Errorf("failed to get REST client config: %v", err)
	}

	clientset, err := clientset.NewForConfig(config)
	if err != nil {
		return false, fmt.Errorf("failed to create ClientSet: %v", err)
	}

	patched, dockerEndpoint, err := patchKubeletConfig(bootConfig.ConfigzBaseUrl, bootConfig.StatsBaseUrl, bootConfig.SavedConfigPath, clientset)
	if err != nil {
		return false, err
	}
	if !patched {
		return false, nil
	}

	_, err = installCriProxyContainer(dockerEndpoint, dockerEndpoint, bootConfig.ProxyPath, bootConfig.ProxyArgs)

	if err != nil {
		return false, err
	}

	err = waitForSocket(bootConfig.ProxySocketPath)
	return err == nil, err
}

func LoadKubeletConfig(path string) (*cfg.KubeletConfiguration, error) {
	bs, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg cfg.KubeletConfiguration
	if err := json.Unmarshal(bs, &cfg); err != nil {
		return nil, fmt.Errorf("failed to load kubelet config from %q: %v", path, err)
	}
	return &cfg, err
}
