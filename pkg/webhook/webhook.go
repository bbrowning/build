/*
Copyright 2017 Google Inc. All Rights Reserved.
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

package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"

	duckv1alpha1 "github.com/knative/pkg/apis/duck/v1alpha1"
	pkgwebhook "github.com/knative/pkg/webhook"
	"github.com/mattbaird/jsonpatch"
	"go.uber.org/zap"
	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	corev1 "k8s.io/api/core/v1"
	v1beta1 "k8s.io/api/extensions/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientadmissionregistrationv1beta1 "k8s.io/client-go/kubernetes/typed/admissionregistration/v1beta1"

	"github.com/knative/build/pkg"
	"github.com/knative/build/pkg/apis/build"
	"github.com/knative/build/pkg/apis/build/v1alpha1"
	"github.com/knative/build/pkg/builder"
	buildclientset "github.com/knative/build/pkg/client/clientset/versioned"
	"github.com/knative/pkg/logging"
	"github.com/knative/pkg/logging/logkey"
)

const (
	knativeAPIVersion = "v1alpha1"
	secretServerKey   = "server-key.pem"
	secretServerCert  = "server-cert.pem"
	secretCACert      = "ca-cert.pem"
	// TODO: Could these come from somewhere else.
	buildWebhookDeployment = "build-webhook"
)

var resources = []string{"builds", "buildtemplates", "clusterbuildtemplates"}

// genericCRDHandler defines the factory object to use for unmarshaling incoming objects
type genericCRDHandler struct {
	Factory runtime.Object

	// Defaulter sets defaults on an object. If non-nil error is returned, object
	// creation is denied. Mutations should be appended to the patches operations.
	Defaulter func(ctx context.Context, patches *[]jsonpatch.JsonPatchOperation, crd pkgwebhook.GenericCRD) error

	// Validator validates an object, mutating it if necessary. If non-nil error
	// is returned, object creation is denied. Mutations should be appended to
	// the patches operations.
	Validator func(ctx context.Context, patches *[]jsonpatch.JsonPatchOperation, old, new pkgwebhook.GenericCRD) error
}

// AdmissionController implements the external admission webhook for validation of
// pilot configuration.
type AdmissionController struct {
	client      kubernetes.Interface
	buildClient buildclientset.Interface
	builder     builder.Interface
	options     pkgwebhook.ControllerOptions
	handlers    map[string]genericCRDHandler
	logger      *zap.SugaredLogger
}

var _ pkgwebhook.GenericCRD = (*v1alpha1.Build)(nil)
var _ pkgwebhook.GenericCRD = (*v1alpha1.BuildTemplate)(nil)
var _ pkgwebhook.GenericCRD = (*v1alpha1.ClusterBuildTemplate)(nil)

// getAPIServerExtensionCACert gets the Kubernetes aggregate apiserver
// client CA cert used by validator.
//
// NOTE: this certificate is provided kubernetes. We do not control
// its name or location.
func getAPIServerExtensionCACert(cl kubernetes.Interface) ([]byte, error) {
	const name = "extension-apiserver-authentication"
	c, err := cl.CoreV1().ConfigMaps(metav1.NamespaceSystem).Get(name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	pem, ok := c.Data["requestheader-client-ca-file"]
	if !ok {
		return nil, fmt.Errorf("cannot find ca.crt in %v: ConfigMap.Data is %#v", name, c.Data)
	}
	return []byte(pem), nil
}

// MakeTLSConfig makes a TLS configuration suitable for use with the server
func makeTLSConfig(serverCert, serverKey, caCert []byte) (*tls.Config, error) {
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)
	cert, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    caCertPool,
		ClientAuth:   tls.NoClientCert,
		// Note on GKE there apparently is no client cert sent, so this
		// does not work on GKE.
		// TODO: make this into a configuration option.
		//		ClientAuth:   tls.RequireAndVerifyClientCert,
	}, nil
}

func getOrGenerateKeyCertsFromSecret(ctx context.Context, client kubernetes.Interface, name,
	namespace string) (serverKey, serverCert, caCert []byte, err error) {
	logger := logging.FromContext(ctx)
	secret, err := client.CoreV1().Secrets(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, nil, nil, err
		}
		logger.Info("Did not find existing secret, creating one")
		newSecret, err := generateSecret(ctx, name, namespace)
		if err != nil {
			return nil, nil, nil, err
		}
		secret, err = client.CoreV1().Secrets(namespace).Create(newSecret)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, nil, nil, err
		}
		// Ok, so something else might have created, try fetching it one more time
		secret, err = client.CoreV1().Secrets(namespace).Get(name, metav1.GetOptions{})
		if err != nil {
			return nil, nil, nil, err
		}
	}

	var ok bool
	if serverKey, ok = secret.Data[secretServerKey]; !ok {
		return nil, nil, nil, errors.New("server key missing")
	}
	if serverCert, ok = secret.Data[secretServerCert]; !ok {
		return nil, nil, nil, errors.New("server cert missing")
	}
	if caCert, ok = secret.Data[secretCACert]; !ok {
		return nil, nil, nil, errors.New("ca cert missing")
	}
	return serverKey, serverCert, caCert, nil
}

// NewAdmissionController creates a new instance of the admission webhook controller.
func NewAdmissionController(client kubernetes.Interface, buildClient buildclientset.Interface, builder builder.Interface, options pkgwebhook.ControllerOptions, logger *zap.SugaredLogger) *AdmissionController {
	ac := &AdmissionController{
		client:      client,
		buildClient: buildClient,
		builder:     builder,
		options:     options,
		logger:      logger,
	}
	ac.handlers = map[string]genericCRDHandler{
		"Build": {
			Factory:   &v1alpha1.Build{},
			Validator: ac.validateBuild,
		},
		"BuildTemplate": {
			Factory:   &v1alpha1.BuildTemplate{},
			Validator: ac.validateBuildTemplate,
		},
		"ClusterBuildTemplate": {
			Factory:   &v1alpha1.ClusterBuildTemplate{},
			Validator: ac.validateClusterBuildTemplate,
		},
	}
	return ac
}

func configureCerts(ctx context.Context, client kubernetes.Interface, options *pkgwebhook.ControllerOptions) (*tls.Config, []byte, error) {
	apiServerCACert, err := getAPIServerExtensionCACert(client)
	if err != nil {
		return nil, nil, err
	}
	serverKey, serverCert, caCert, err := getOrGenerateKeyCertsFromSecret(
		ctx, client, options.SecretName, options.Namespace)
	if err != nil {
		return nil, nil, err
	}
	tlsConfig, err := makeTLSConfig(serverCert, serverKey, apiServerCACert)
	if err != nil {
		return nil, nil, err
	}
	return tlsConfig, caCert, nil
}

// Run implements the admission controller run loop.
func (ac *AdmissionController) Run(stop <-chan struct{}) error {
	logger := ac.logger
	ctx := logging.WithLogger(context.TODO(), logger)
	tlsConfig, caCert, err := configureCerts(ctx, ac.client, &ac.options)
	if err != nil {
		logger.Error("Could not configure admission webhook certs", zap.Error(err))
		return err
	}

	server := &http.Server{
		Handler:   ac,
		Addr:      fmt.Sprintf(":%v", ac.options.Port),
		TLSConfig: tlsConfig,
	}

	logger.Info("Found certificates for webhook...")
	if ac.options.RegistrationDelay != 0 {
		logger.Infof("Delaying admission webhook registration for %v", ac.options.RegistrationDelay)
	}

	select {
	case <-time.After(ac.options.RegistrationDelay):
		cl := ac.client.AdmissionregistrationV1beta1().MutatingWebhookConfigurations()
		if err := ac.register(ctx, cl, caCert); err != nil {
			logger.Error("Failed to register webhook", zap.Error(err))
			return err
		}
		defer func() {
			if err := ac.unregister(ctx, cl); err != nil {
				logger.Error("Failed to unregister webhook", zap.Error(err))
			}
		}()
		logger.Info("Successfully registered webhook")
	case <-stop:
		return nil
	}

	go func() {
		if err := server.ListenAndServeTLS("", ""); err != nil {
			logger.Error("ListenAndServeTLS for admission webhook returned error", zap.Error(err))
		}
	}()
	<-stop
	server.Close() // nolint: errcheck
	return nil
}

// unregister unregisters the external admission webhook
func (ac *AdmissionController) unregister(
	ctx context.Context, client clientadmissionregistrationv1beta1.MutatingWebhookConfigurationInterface) error {
	logger := logging.FromContext(ctx)
	logger.Info("Exiting..")
	return nil
}

func (ac *AdmissionController) register(
	ctx context.Context, client clientadmissionregistrationv1beta1.MutatingWebhookConfigurationInterface, caCert []byte) error { // nolint: lll
	logger := logging.FromContext(ctx)

	// Set the owner to our deployment
	deployment, err := ac.client.ExtensionsV1beta1().Deployments(pkg.GetBuildSystemNamespace()).Get(buildWebhookDeployment, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("Failed to fetch our deployment: %s", err)
	}
	deploymentRef := metav1.NewControllerRef(deployment, v1beta1.SchemeGroupVersion.WithKind("Deployment"))

	webhook := &admissionregistrationv1beta1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:            ac.options.WebhookName,
			OwnerReferences: []metav1.OwnerReference{*deploymentRef},
		},
		Webhooks: []admissionregistrationv1beta1.Webhook{{
			Name: ac.options.WebhookName,
			Rules: []admissionregistrationv1beta1.RuleWithOperations{{
				Operations: []admissionregistrationv1beta1.OperationType{
					admissionregistrationv1beta1.Create,
					admissionregistrationv1beta1.Update,
				},
				Rule: admissionregistrationv1beta1.Rule{
					APIGroups:   []string{build.GroupName},
					APIVersions: []string{knativeAPIVersion},
					Resources:   resources,
				},
			}},
			ClientConfig: admissionregistrationv1beta1.WebhookClientConfig{
				Service: &admissionregistrationv1beta1.ServiceReference{
					Namespace: ac.options.Namespace,
					Name:      ac.options.ServiceName,
				},
				CABundle: caCert,
			},
		}},
	}

	// Try to create the webhook and if it already exists validate webhook rules
	if _, err := client.Create(webhook); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("Failed to create a webhook: %s", err)
		}
		logger.Info("Webhook already exists")
		configuredWebhook, err := client.Get(ac.options.WebhookName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("Error retrieving webhook: %s", err)
		}
		if !reflect.DeepEqual(configuredWebhook.Webhooks, webhook.Webhooks) {
			logger.Info("Updating webhook")
			// Set the ResourceVersion as required by update.
			webhook.ObjectMeta.ResourceVersion = configuredWebhook.ObjectMeta.ResourceVersion
			if _, err := client.Update(webhook); err != nil {
				return fmt.Errorf("Failed to update webhook: %s", err)
			}
		} else {
			logger.Info("Webhook is already valid")
		}
	} else {
		logger.Info("Created a webhook")
	}
	return nil
}

// ServeHTTP implements the external admission webhook for mutating
// ela resources.
func (ac *AdmissionController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logger := ac.logger
	logger.Infof("Webhook ServeHTTP request=%#v", r)

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		http.Error(w, "invalid Content-Type, want `application/json`", http.StatusUnsupportedMediaType)
		return
	}

	var review admissionv1beta1.AdmissionReview
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
		http.Error(w, fmt.Sprintf("could not decode body: %v", err), http.StatusBadRequest)
		return
	}

	logger = logger.With(
		zap.String(logkey.Kind, fmt.Sprint(review.Request.Kind)),
		zap.String(logkey.Namespace, review.Request.Namespace),
		zap.String(logkey.Name, review.Request.Name),
		zap.String(logkey.Operation, fmt.Sprint(review.Request.Operation)),
		zap.String(logkey.Resource, fmt.Sprint(review.Request.Resource)),
		zap.String(logkey.SubResource, fmt.Sprint(review.Request.SubResource)),
		zap.String(logkey.UserInfo, fmt.Sprint(review.Request.UserInfo)))
	reviewResponse := ac.admit(logging.WithLogger(r.Context(), logger), review.Request)
	var response admissionv1beta1.AdmissionReview
	if reviewResponse != nil {
		response.Response = reviewResponse
		response.Response.UID = review.Request.UID
	}

	logger.Infof("AdmissionReview for %s: %v/%v response=%v",
		review.Request.Kind, review.Request.Namespace, review.Request.Name, reviewResponse)

	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, fmt.Sprintf("could encode response: %v", err), http.StatusInternalServerError)
		return
	}
}

func makeErrorStatus(reason string, args ...interface{}) *admissionv1beta1.AdmissionResponse {
	result := apierrors.NewBadRequest(fmt.Sprintf(reason, args...)).Status()
	return &admissionv1beta1.AdmissionResponse{
		Result:  &result,
		Allowed: false,
	}
}

func (ac *AdmissionController) admit(ctx context.Context, request *admissionv1beta1.AdmissionRequest) *admissionv1beta1.AdmissionResponse {
	logger := logging.FromContext(ctx)
	switch request.Operation {
	case admissionv1beta1.Create, admissionv1beta1.Update:
	default:
		logger.Infof("Unhandled webhook operation, letting it through %v", request.Operation)
		return &admissionv1beta1.AdmissionResponse{Allowed: true}
	}

	patchBytes, err := ac.mutate(ctx, request.Kind.Kind, request.OldObject.Raw, request.Object.Raw)
	if err != nil {
		return makeErrorStatus("mutation failed: %v", err)
	}
	logger.Infof("Kind: %q PatchBytes: %v", request.Kind, string(patchBytes))

	return &admissionv1beta1.AdmissionResponse{
		Patch:   patchBytes,
		Allowed: true,
		PatchType: func() *admissionv1beta1.PatchType {
			pt := admissionv1beta1.PatchTypeJSONPatch
			return &pt
		}(),
	}
}

func (ac *AdmissionController) mutate(ctx context.Context, kind string, oldBytes []byte, newBytes []byte) ([]byte, error) {
	logger := logging.FromContext(ctx)
	handler, ok := ac.handlers[kind]
	if !ok {
		logger.Errorf("Unhandled kind %q", kind)
		return nil, fmt.Errorf("unhandled kind: %q", kind)
	}

	oldObj := handler.Factory.DeepCopyObject().(pkgwebhook.GenericCRD)
	newObj := handler.Factory.DeepCopyObject().(pkgwebhook.GenericCRD)

	if len(newBytes) != 0 {
		newDecoder := json.NewDecoder(bytes.NewBuffer(newBytes))
		newDecoder.DisallowUnknownFields()
		if err := newDecoder.Decode(&newObj); err != nil {
			return nil, fmt.Errorf("cannot decode incoming new object: %v", err)
		}
	} else {
		// Use nil to denote the absence of a new object (delete)
		newObj = nil
	}

	if len(oldBytes) != 0 {
		oldDecoder := json.NewDecoder(bytes.NewBuffer(oldBytes))
		oldDecoder.DisallowUnknownFields()
		if err := oldDecoder.Decode(&oldObj); err != nil {
			return nil, fmt.Errorf("cannot decode incoming old object: %v", err)
		}
	} else {
		// Use nil to denote the absence of an old object (create)
		oldObj = nil
	}

	var patches []jsonpatch.JsonPatchOperation

	err := updateGeneration(ctx, &patches, oldObj, newObj)
	if err != nil {
		logger.Error("Failed to update generation", zap.Error(err))
		return nil, fmt.Errorf("Failed to update generation: %s", err)
	}

	if defaulter := handler.Defaulter; defaulter != nil {
		if err := defaulter(ctx, &patches, newObj); err != nil {
			logger.Error("Failed the resource specific defaulter", zap.Error(err))
			// Return the error message as-is to give the defaulter callback
			// discretion over (our portion of) the message that the user sees.
			return nil, err
		}
	}

	if err := handler.Validator(ctx, &patches, oldObj, newObj); err != nil {
		logger.Error("Failed the resource specific validation", zap.Error(err))
		// Return the error message as-is to give the validation callback
		// discretion over (our portion of) the message that the user sees.
		return nil, err
	}

	return json.Marshal(patches)
}

// updateGeneration sets the generation by following this logic:
// if there's no old object, it's create, set generation to 1
// if there's an old object and spec has changed, set generation to oldGeneration + 1
// appends the patch to patches if changes are necessary.
// TODO: Generation does not work correctly with CRD. They are scrubbed
// by the APIserver (https://github.com/kubernetes/kubernetes/issues/58778)
// So, we add Generation here. Once that gets fixed, remove this and use
// ObjectMeta.Generation instead.
func updateGeneration(ctx context.Context, patches *[]jsonpatch.JsonPatchOperation, old, new pkgwebhook.GenericCRD) error {
	logger := logging.FromContext(ctx)
	var oldGeneration *duckv1alpha1.Generational
	var err error
	if old == nil {
		logger.Info("Old is nil")
	} else {
		oldGeneration, err = asGenerational(ctx, old)
		if err != nil {
			return err
		}
	}
	if oldGeneration.Spec.Generation == 0 {
		logger.Info("Creating an object, setting generation to 1")
		*patches = append(*patches, jsonpatch.JsonPatchOperation{
			Operation: "add",
			Path:      "/spec/generation",
			Value:     1,
		})
		return nil
	}
	oldSpecJSON, err := getSpecJSON(old)
	if err != nil {
		logger.Error("Failed to get Spec JSON for old", zap.Error(err))
	}
	newSpecJSON, err := getSpecJSON(new)
	if err != nil {
		logger.Error("Failed to get Spec JSON for new", zap.Error(err))
	}

	specPatches, err := jsonpatch.CreatePatch(oldSpecJSON, newSpecJSON)
	if err != nil {
		fmt.Printf("Error creating JSON patch:%v", err)
		return err
	}

	if len(specPatches) > 0 {
		specPatchesJSON, err := json.Marshal(specPatches)
		if err != nil {
			logger.Error("Failed to marshal spec patches", zap.Error(err))
			return err
		}
		logger.Infof("Specs differ:\n%+v\n", string(specPatchesJSON))

		operation := "replace"
		newGeneration, err := asGenerational(ctx, new)
		if err != nil {
			return err
		}
		if newGeneration.Spec.Generation == 0 {
			// If new is missing Generation, we need to "add" instead of "replace".
			// We see this for Service resources because the initial generation is
			// added to the managed Configuration and Route, but not the Service
			// that manages them.
			// TODO(#642): Remove this.
			operation = "add"
		}
		*patches = append(*patches, jsonpatch.JsonPatchOperation{
			Operation: operation,
			Path:      "/spec/generation",
			Value:     oldGeneration.Spec.Generation + 1,
		})
		return nil
	}
	logger.Info("No changes in the spec, not bumping generation")
	return nil
}

func asGenerational(ctx context.Context, crd pkgwebhook.GenericCRD) (*duckv1alpha1.Generational, error) {
	raw, err := json.Marshal(crd)
	if err != nil {
		return nil, err
	}
	kr := &duckv1alpha1.Generational{}
	if err := json.Unmarshal(raw, kr); err != nil {
		return nil, err
	}
	return kr, nil
}

func generateSecret(ctx context.Context, name, namespace string) (*corev1.Secret, error) {
	serverKey, serverCert, caCert, err := pkgwebhook.CreateCerts(ctx, name, namespace)
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			secretServerKey:  serverKey,
			secretServerCert: serverCert,
			secretCACert:     caCert,
		},
	}, nil
}

// Not worth fully duck typing since there's no shared schema.
type hasSpec struct {
	Spec json.RawMessage `json:"spec"`
}

func getSpecJSON(crd pkgwebhook.GenericCRD) ([]byte, error) {
	b, err := json.Marshal(crd)
	if err != nil {
		return nil, err
	}
	hs := hasSpec{}
	if err := json.Unmarshal(b, &hs); err != nil {
		return nil, err
	}
	return []byte(hs.Spec), nil
}
