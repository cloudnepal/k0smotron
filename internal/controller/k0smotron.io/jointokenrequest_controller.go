/*
Copyright 2023.

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

package k0smotronio

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	km "github.com/k0sproject/k0smotron/api/k0smotron.io/v1beta1"
	"github.com/k0sproject/k0smotron/internal/controller/util"
	"github.com/k0sproject/k0smotron/internal/exec"
)

// JoinTokenRequestReconciler reconciles a JoinTokenRequest object
type JoinTokenRequestReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	ClientSet  *kubernetes.Clientset
	RESTConfig *rest.Config
}

//+kubebuilder:rbac:groups=k0smotron.io,resources=jointokenrequests,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=k0smotron.io,resources=jointokenrequests/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=k0smotron.io,resources=jointokenrequests/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete

func (r *JoinTokenRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var jtr km.JoinTokenRequest
	if err := r.Get(ctx, req.NamespacedName, &jtr); err != nil {
		logger.Error(err, "unable to fetch JoinTokenRequest")
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var cluster km.Cluster
	err := r.Client.Get(ctx, types.NamespacedName{Name: jtr.Spec.ClusterRef.Name, Namespace: jtr.Spec.ClusterRef.Namespace}, &cluster)
	if err != nil {
		r.updateStatus(ctx, jtr, "Failed getting cluster")
		return ctrl.Result{Requeue: true, RequeueAfter: time.Minute}, err
	}
	jtr.Status.ClusterUID = cluster.GetUID()

	logger.Info("Reconciling")
	pod, err := util.FindStatefulSetPod(ctx, r.ClientSet, km.GetStatefulSetName(jtr.Spec.ClusterRef.Name), jtr.Spec.ClusterRef.Namespace)
	if err != nil {
		r.updateStatus(ctx, jtr, "Failed finding pods in statefulset")
		return ctrl.Result{Requeue: true, RequeueAfter: time.Minute}, err
	}

	finalizerName := "jointokenrequests.k0smotron.io/finalizer"
	if !jtr.ObjectMeta.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&jtr, finalizerName) {
			if err := r.invalidateToken(ctx, &jtr, pod); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(&jtr, finalizerName)
			if err := r.Update(ctx, &jtr); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&jtr, finalizerName) {
		controllerutil.AddFinalizer(&jtr, finalizerName)
	}

	if jtr.Status.TokenID != "" {
		logger.Info("Already reconciled")
		return ctrl.Result{}, nil
	}

	cmd := fmt.Sprintf("k0s token create --role=%s --expiry=%s", jtr.Spec.Role, jtr.Spec.Expiry)
	token, err := exec.PodExecCmdOutput(ctx, r.ClientSet, r.RESTConfig, pod.Name, pod.Namespace, cmd)
	if err != nil {
		r.updateStatus(ctx, jtr, "Failed getting token")
		return ctrl.Result{Requeue: true, RequeueAfter: time.Minute}, err
	}

	newToken, newKubeconfig, err := ReplaceTokenPort(token, cluster)
	if err != nil {
		r.updateStatus(ctx, jtr, "Failed update token URL")
		return ctrl.Result{Requeue: true, RequeueAfter: time.Minute}, err
	}

	if err := r.reconcileSecret(ctx, jtr, newToken); err != nil {
		r.updateStatus(ctx, jtr, "Failed creating secret")
		return ctrl.Result{Requeue: true, RequeueAfter: time.Minute}, err
	}

	tokenID, err := getTokenID(newKubeconfig, jtr.Spec.Role)
	if err != nil {
		r.updateStatus(ctx, jtr, "Failed getting token id")
		return ctrl.Result{Requeue: true, RequeueAfter: time.Minute}, err
	}
	jtr.Status.TokenID = tokenID
	r.updateStatus(ctx, jtr, "Reconciliation successful")
	return ctrl.Result{}, nil
}

func (r *JoinTokenRequestReconciler) invalidateToken(ctx context.Context, jtr *km.JoinTokenRequest, pod *v1.Pod) error {
	cmd := fmt.Sprintf("k0s token invalidate %s", jtr.Status.TokenID)
	_, err := exec.PodExecCmdOutput(ctx, r.ClientSet, r.RESTConfig, pod.Name, pod.Namespace, cmd)
	return err
}

func (r *JoinTokenRequestReconciler) reconcileSecret(ctx context.Context, jtr km.JoinTokenRequest, token string) error {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling configmap")

	cm, err := r.generateSecret(&jtr, token)
	if err != nil {
		return err
	}

	return r.Client.Patch(ctx, &cm, client.Apply, patchOpts...)
}

func (r *JoinTokenRequestReconciler) generateSecret(jtr *km.JoinTokenRequest, token string) (v1.Secret, error) {
	labels := map[string]string{
		clusterLabel:                 jtr.Spec.ClusterRef.Name,
		"k0smotron.io/cluster-uid":   string(jtr.Status.ClusterUID),
		"k0smotron.io/role":          jtr.Spec.Role,
		"k0smotron.io/token-request": jtr.Name,
	}
	for k, v := range jtr.Labels {
		labels[k] = v
	}
	secret := v1.Secret{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        jtr.Name,
			Namespace:   jtr.Namespace,
			Labels:      labels,
			Annotations: jtr.Annotations,
		},
		StringData: map[string]string{
			"token": token,
		},
	}

	_ = ctrl.SetControllerReference(jtr, &secret, r.Scheme)
	return secret, nil
}

func (r *JoinTokenRequestReconciler) updateStatus(ctx context.Context, jtr km.JoinTokenRequest, status string) {
	logger := log.FromContext(ctx)
	jtr.Status.ReconciliationStatus = status
	if err := r.Status().Update(ctx, &jtr); err != nil {
		logger.Error(err, fmt.Sprintf("Unable to update status: %s", status))
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *JoinTokenRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&km.JoinTokenRequest{}).
		Complete(r)
}

func replaceKubeconfigPort(in string, cluster km.Cluster) (string, *api.Config, error) {
	cfg, err := clientcmd.Load([]byte(in))
	if err != nil {
		return "", nil, err
	}

	u, err := url.Parse(cfg.Clusters["k0s"].Server)
	if err != nil {
		return "", nil, err
	}
	parts := strings.Split(u.Host, ":")
	u.Host = fmt.Sprintf("%s:%d", parts[0], cluster.Spec.Service.APIPort)

	cfg.Clusters["k0s"].Server = u.String()

	b, err := clientcmd.Write(*cfg)
	if err != nil {
		return "", nil, err
	}

	return string(b), cfg, nil
}

func ReplaceTokenPort(token string, cluster km.Cluster) (string, *api.Config, error) {
	b, err := tokenDecode(token)
	if err != nil {
		return "", nil, err
	}

	updatedKubeconfig, cfg, err := replaceKubeconfigPort(string(b), cluster)
	if err != nil {
		return "", nil, err
	}

	newToken, err := tokenEncode([]byte(updatedKubeconfig))

	return newToken, cfg, err
}

func getTokenID(cfg *api.Config, role string) (string, error) {
	var userName string
	switch role {
	case "controller":
		userName = "controller-bootstrap"
	case "worker":
		userName = "kubelet-bootstrap"
	default:
		return "", fmt.Errorf("unknown role: %s", role)
	}

	tokenID, _, _ := strings.Cut(cfg.AuthInfos[userName].Token, ".")
	return tokenID, nil
}

func tokenDecode(token string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}

	output, err := io.ReadAll(gz)
	if err != nil {
		return nil, err
	}

	return output, err
}

func tokenEncode(token []byte) (string, error) {
	in := bytes.NewReader(token)

	var outBuf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&outBuf, gzip.BestCompression)
	if err != nil {
		return "", err
	}

	_, err = io.Copy(gz, in)
	gzErr := gz.Close()
	if err != nil {
		return "", err
	}
	if gzErr != nil {
		return "", gzErr
	}

	return base64.StdEncoding.EncodeToString(outBuf.Bytes()), nil
}
