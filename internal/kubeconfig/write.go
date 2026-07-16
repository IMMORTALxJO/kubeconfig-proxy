package kubeconfig

import (
	"os"
	"path/filepath"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const execAPIVersion = "client.authentication.k8s.io/v1"

func AddProxyContext(path, contextName, serverURL, namespace, command, statePath string, certificateAuthorityData []byte) error {
	config, err := clientcmd.LoadFromFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		config = clientcmdapi.NewConfig()
	}

	clusterName := "kubeconfig-proxy/" + contextName
	authName := "kubeconfig-proxy/" + contextName
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
