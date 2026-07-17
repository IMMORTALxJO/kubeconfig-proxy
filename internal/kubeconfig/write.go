package kubeconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const execAPIVersion = "client.authentication.k8s.io/v1"
const proxyEntryPrefix = "kubeconfig-proxy/"

func AddProxyContext(path, contextName, serverURL, namespace, command, statePath string, certificateAuthorityData []byte) error {
	config, err := clientcmd.LoadFromFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		config = clientcmdapi.NewConfig()
	}

	clusterName := proxyEntryName(contextName)
	authName := proxyEntryName(contextName)
	config.Clusters[clusterName] = &clientcmdapi.Cluster{
		Server:                   serverURL,
		CertificateAuthorityData: certificateAuthorityData,
	}
	config.AuthInfos[authName] = &clientcmdapi.AuthInfo{
		Exec: &clientcmdapi.ExecConfig{
			APIVersion:      execAPIVersion,
			Command:         command,
			Args:            []string{"credential", "--state", statePath},
			InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
		},
	}
	config.Contexts[contextName] = &clientcmdapi.Context{
		Cluster:   clusterName,
		AuthInfo:  authName,
		Namespace: namespace,
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func DeleteProxyContext(path, contextName string) ([]string, error) {
	config, err := clientcmd.LoadFromFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	entryName := proxyEntryName(contextName)
	statePaths := make([]string, 0, 1)
	changed := false

	if context := config.Contexts[contextName]; context != nil {
		if context.Cluster != entryName || context.AuthInfo != entryName {
			return nil, fmt.Errorf("context %q is not managed by kubeconfig-proxy", contextName)
		}
		statePaths = appendStatePaths(statePaths, authInfoStatePaths(config.AuthInfos[context.AuthInfo])...)
		delete(config.Contexts, contextName)
		if config.CurrentContext == contextName {
			config.CurrentContext = ""
		}
		changed = true
	}

	if authInfo := config.AuthInfos[entryName]; authInfo != nil {
		statePaths = appendStatePaths(statePaths, authInfoStatePaths(authInfo)...)
		delete(config.AuthInfos, entryName)
		changed = true
	}
	if _, ok := config.Clusters[entryName]; ok {
		delete(config.Clusters, entryName)
		changed = true
	}

	if !changed {
		return statePaths, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		return nil, err
	}
	return statePaths, os.Chmod(path, 0o600)
}

func proxyEntryName(contextName string) string {
	return proxyEntryPrefix + contextName
}

func authInfoStatePaths(authInfo *clientcmdapi.AuthInfo) []string {
	if authInfo == nil || authInfo.Exec == nil {
		return nil
	}
	args := authInfo.Exec.Args
	paths := make([]string, 0, 1)
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--state" && i+1 < len(args):
			paths = append(paths, args[i+1])
			i++
		case strings.HasPrefix(args[i], "--state="):
			paths = append(paths, strings.TrimPrefix(args[i], "--state="))
		}
	}
	return paths
}

func appendStatePaths(paths []string, values ...string) []string {
	for _, value := range values {
		if value == "" {
			continue
		}
		seen := false
		for _, path := range paths {
			if path == value {
				seen = true
				break
			}
		}
		if !seen {
			paths = append(paths, value)
		}
	}
	return paths
}
