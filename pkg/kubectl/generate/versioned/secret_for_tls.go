/*
Copyright 2015 The Kubernetes Authors.

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

package versioned

import (
	"crypto/tls"
	"fmt"
	"io/ioutil"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/kubectl/generate"
	"k8s.io/kubernetes/pkg/kubectl/util/hash"
)

// SecretForTLSGeneratorV1 supports stable generation of a TLS secret.
type SecretForTLSGeneratorV1 struct {
	// Name is the name of this TLS secret.
	Name string
	// Key is the path to the user's private key.
	Key string
	// Cert is the path to the user's public key certificate.
	Cert string
	// AppendHash; if true, derive a hash from the Secret and append it to the name
	AppendHash bool
}

// Ensure it supports the generator pattern that uses parameter injection
var _ generate.Generator = &SecretForTLSGeneratorV1{}

// Ensure it supports the generator pattern that uses parameters specified during construction
var _ generate.StructuredGenerator = &SecretForTLSGeneratorV1{}

// Generate returns a secret using the specified parameters
func (s SecretForTLSGeneratorV1) Generate(genericParams map[string]interface{}) (runtime.Object, error) {
	err := generate.ValidateParams(s.ParamNames(), genericParams)
	if err != nil {
		return nil, err
	}
	delegate := &SecretForTLSGeneratorV1{}
	hashParam, found := genericParams["append-hash"]
	if found {
		hashBool, isBool := hashParam.(bool)
		if !isBool {
			return nil, fmt.Errorf("expected bool, found :%v", hashParam)
		}
		delegate.AppendHash = hashBool
		delete(genericParams, "append-hash")
	}
	params := map[string]string{}
	for key, value := range genericParams {
		strVal, isString := value.(string)
		if !isString {
			return nil, fmt.Errorf("expected string, saw %v for '%s'", value, key)
		}
		params[key] = strVal
	}
	delegate.Name = params["name"]
	delegate.Key = params["key"]
	delegate.Cert = params["cert"]
	return delegate.StructuredGenerate()
}

// StructuredGenerate outputs a secret object using the configured fields
func (s SecretForTLSGeneratorV1) StructuredGenerate() (runtime.Object, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	tlsCrt, err := readFile(s.Cert)
	if err != nil {
		return nil, err
	}
	tlsKey, err := readFile(s.Key)
	if err != nil {
		return nil, err
	}

	if _, err := tls.X509KeyPair(tlsCrt, tlsKey); err != nil {
		return nil, fmt.Errorf("failed to load key pair %v", err)
	}
	// TODO: Add more validation.
	// 1. If the certificate contains intermediates, it is a valid chain.
	// 2. Format etc.

	secret := &v1.Secret{}
	secret.Name = s.Name
	secret.Type = v1.SecretTypeTLS
	secret.Data = map[string][]byte{}
	secret.Data[v1.TLSCertKey] = []byte(tlsCrt)
	secret.Data[v1.TLSPrivateKeyKey] = []byte(tlsKey)
	if s.AppendHash {
		h, err := hash.SecretHash(secret)
		if err != nil {
			return nil, err
		}
		secret.Name = fmt.Sprintf("%s-%s", secret.Name, h)
	}
	return secret, nil
}

// readFile just reads a file into a byte array.
func readFile(file string) ([]byte, error) {
	b, err := ioutil.ReadFile(file)
	if err != nil {
		return []byte{}, fmt.Errorf("cannot read file %v, %v", file, err)
	}
	return b, nil
}

// ParamNames returns the set of supported input parameters when using the parameter injection generator pattern
func (s SecretForTLSGeneratorV1) ParamNames() []generate.GeneratorParam {
	return []generate.GeneratorParam{
		{Name: "name", Required: true},
		{Name: "key", Required: true},
		{Name: "cert", Required: true},
		{Name: "append-hash", Required: false},
	}
}

// validate validates required fields are set to support structured generation
func (s SecretForTLSGeneratorV1) validate() error {
	// TODO: This is not strictly necessary. We can generate a self signed cert
	// if no key/cert is given. The only requirement is that we either get both
	// or none. See test/e2e/ingress_utils for self signed cert generation.
	if len(s.Key) == 0 {
		return fmt.Errorf("key must be specified")
	}
	if len(s.Cert) == 0 {
		return fmt.Errorf("certificate must be specified")
	}
	return nil
}
