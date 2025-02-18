/*
Copyright 2016 The Kubernetes Authors.

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

package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/golang/glog"

	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	certificates "k8s.io/client-go/kubernetes/typed/certificates/v1beta1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/transport"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/certificate"
	"k8s.io/client-go/util/certificate/csr"
)

const tmpPrivateKeyFile = "kubelet-client.key.tmp"

// LoadClientCert requests a client cert for kubelet if the kubeconfigPath file does not exist.
// The kubeconfig at bootstrapPath is used to request a client certificate from the API server.
// On success, a kubeconfig file referencing the generated key and obtained certificate is written to kubeconfigPath.
// The certificate and key file are stored in certDir.
func LoadClientCert(kubeconfigPath string, bootstrapPath string, certDir string, nodeName types.NodeName) error {
	// Short-circuit if the kubeconfig file exists and is valid.
	ok, err := verifyBootstrapClientConfig(kubeconfigPath)
	if err != nil {
		return err
	}
	if ok {
		glog.V(2).Infof("Kubeconfig %s exists and is valid, skipping bootstrap", kubeconfigPath)
		return nil
	}

	glog.V(2).Info("Using bootstrap kubeconfig to generate TLS client cert, key and kubeconfig file")

	bootstrapClientConfig, err := loadRESTClientConfig(bootstrapPath)
	if err != nil {
		return fmt.Errorf("unable to load bootstrap kubeconfig: %v", err)
	}

	bootstrapClient, err := certificates.NewForConfig(bootstrapClientConfig)
	if err != nil {
		return fmt.Errorf("unable to create certificates signing request client: %v", err)
	}

	store, err := certificate.NewFileStore("kubelet-client", certDir, certDir, "", "")
	if err != nil {
		return fmt.Errorf("unable to build bootstrap cert store")
	}

	var keyData []byte
	if cert, err := store.Current(); err == nil {
		if cert.PrivateKey != nil {
			keyData, err = certutil.MarshalPrivateKeyToPEM(cert.PrivateKey)
			if err != nil {
				keyData = nil
			}
		}
	}
	// Cache the private key in a separate file until CSR succeeds. This has to
	// be a separate file because store.CurrentPath() points to a symlink
	// managed by the store.
	privKeyPath := filepath.Join(certDir, tmpPrivateKeyFile)
	if !verifyKeyData(keyData) {
		glog.V(2).Infof("No valid private key and/or certificate found, reusing existing private key or creating a new one")
		// Note: always call LoadOrGenerateKeyFile so that private key is
		// reused on next startup if CSR request fails.
		keyData, _, err = certutil.LoadOrGenerateKeyFile(privKeyPath)
		if err != nil {
			return err
		}
	}

	if err := waitForServer(*bootstrapClientConfig, 1*time.Minute); err != nil {
		glog.Warningf("Error waiting for apiserver to come up: %v", err)
	}

	certData, err := csr.RequestNodeCertificate(bootstrapClient.CertificateSigningRequests(), keyData, nodeName)
	if err != nil {
		return err
	}
	if _, err := store.Update(certData, keyData); err != nil {
		return err
	}
	if err := os.Remove(privKeyPath); err != nil && !os.IsNotExist(err) {
		glog.V(2).Infof("failed cleaning up private key file %q: %v", privKeyPath, err)
	}

	pemPath := store.CurrentPath()

	// Get the CA data from the bootstrap client config.
	caFile, caData := bootstrapClientConfig.CAFile, []byte{}
	if len(caFile) == 0 {
		caData = bootstrapClientConfig.CAData
	}

	// Build resulting kubeconfig.
	kubeconfigData := clientcmdapi.Config{
		// Define a cluster stanza based on the bootstrap kubeconfig.
		Clusters: map[string]*clientcmdapi.Cluster{"default-cluster": {
			Server:                   bootstrapClientConfig.Host,
			InsecureSkipTLSVerify:    bootstrapClientConfig.Insecure,
			CertificateAuthority:     caFile,
			CertificateAuthorityData: caData,
		}},
		// Define auth based on the obtained client cert.
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"default-auth": {
			ClientCertificate: pemPath,
			ClientKey:         pemPath,
		}},
		// Define a context that connects the auth info and cluster, and set it as the default
		Contexts: map[string]*clientcmdapi.Context{"default-context": {
			Cluster:   "default-cluster",
			AuthInfo:  "default-auth",
			Namespace: "default",
		}},
		CurrentContext: "default-context",
	}

	// Marshal to disk
	return clientcmd.WriteToFile(kubeconfigData, kubeconfigPath)
}

func loadRESTClientConfig(kubeconfig string) (*restclient.Config, error) {
	// Load structured kubeconfig data from the given path.
	loader := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig}
	loadedConfig, err := loader.Load()
	if err != nil {
		return nil, err
	}
	// Flatten the loaded data to a particular restclient.Config based on the current context.
	return clientcmd.NewNonInteractiveClientConfig(
		*loadedConfig,
		loadedConfig.CurrentContext,
		&clientcmd.ConfigOverrides{},
		loader,
	).ClientConfig()
}

// verifyBootstrapClientConfig checks the provided kubeconfig to see if it has a valid
// client certificate. It returns true if the kubeconfig is valid, or an error if bootstrapping
// should stop immediately.
func verifyBootstrapClientConfig(kubeconfigPath string) (bool, error) {
	_, err := os.Stat(kubeconfigPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("error reading existing bootstrap kubeconfig %s: %v", kubeconfigPath, err)
	}
	bootstrapClientConfig, err := loadRESTClientConfig(kubeconfigPath)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to read existing bootstrap client config: %v", err))
		return false, nil
	}
	transportConfig, err := bootstrapClientConfig.TransportConfig()
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to load transport configuration from existing bootstrap client config: %v", err))
		return false, nil
	}
	// has side effect of populating transport config data fields
	if _, err := transport.TLSConfigFor(transportConfig); err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to load TLS configuration from existing bootstrap client config: %v", err))
		return false, nil
	}
	certs, err := certutil.ParseCertsPEM(transportConfig.TLS.CertData)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to load TLS certificates from existing bootstrap client config: %v", err))
		return false, nil
	}
	if len(certs) == 0 {
		utilruntime.HandleError(fmt.Errorf("unable to read TLS certificates from existing bootstrap client config: %v", err))
		return false, nil
	}
	now := time.Now()
	for _, cert := range certs {
		if now.After(cert.NotAfter) {
			utilruntime.HandleError(fmt.Errorf("part of the existing bootstrap client certificate is expired: %s", cert.NotAfter))
			return false, nil
		}
	}
	return true, nil
}

// verifyKeyData returns true if the provided data appears to be a valid private key.
func verifyKeyData(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	_, err := certutil.ParsePrivateKeyPEM(data)
	return err == nil
}

func waitForServer(cfg restclient.Config, deadline time.Duration) error {
	cfg.NegotiatedSerializer = serializer.DirectCodecFactory{CodecFactory: scheme.Codecs}
	cfg.Timeout = 1 * time.Second
	cli, err := restclient.UnversionedRESTClientFor(&cfg)
	if err != nil {
		return fmt.Errorf("couldn't create client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.TODO(), deadline)
	defer cancel()

	var connected bool
	wait.JitterUntil(func() {
		if _, err := cli.Get().AbsPath("/healthz").Do().Raw(); err != nil {
			glog.Infof("Failed to connect to apiserver: %v", err)
			return
		}
		cancel()
		connected = true
	}, 2*time.Second, 0.2, true, ctx.Done())

	if !connected {
		return errors.New("timed out waiting to connect to apiserver")
	}
	return nil
}
