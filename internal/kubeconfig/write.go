package kubeconfig

import (
	"os"
	"path/filepath"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func WriteProxyConfig(path, serverURL, contextName, namespace, bearerToken string, certificateAuthorityData []byte) error {
	config := clientcmdapi.NewConfig()
	config.Clusters["proxy"] = &clientcmdapi.Cluster{
		Server:                   serverURL,
		CertificateAuthorityData: certificateAuthorityData,
	}
	config.AuthInfos["proxy"] = &clientcmdapi.AuthInfo{
		Token: bearerToken,
	}
	config.Contexts[contextName] = &clientcmdapi.Context{
		Cluster:   "proxy",
		AuthInfo:  "proxy",
		Namespace: namespace,
	}
	config.CurrentContext = contextName

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
