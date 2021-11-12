/*
Copyright 2021.

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

package controllers

import (
	"context"
	"fmt"
	"github.com/giantswarm/capa-machinepool-subnet-operator/pkg/subnet"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	expcapa "sigs.k8s.io/cluster-api-provider-aws/exp/api/v1alpha3"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/giantswarm/capa-machinepool-subnet-operator/pkg/awsclient"
	"github.com/giantswarm/capa-machinepool-subnet-operator/pkg/key"
)

// AWSMachinePoolReconciler reconciles a AWSMachinePool object
type AWSMachinePoolReconciler struct {
	DefaultCidrRange  string
	DefaultSubnetSize string

	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsmachinepools,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsmachinepools/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsmachinepools/finalizers,verbs=update

func (r *AWSMachinePoolReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	var err error
	ctx := context.TODO()
	logger := r.Log.WithValues("namespace", req.Namespace, "awsMachinePool", req.Name)

	awsMachinePool := &expcapa.AWSMachinePool{}
	if err := r.Get(ctx, req.NamespacedName, awsMachinePool); err != nil {
		logger.Error(err, "AWSMachinePool does not exist")
		return ctrl.Result{}, err
	}
	// check if CR got CAPI watch-filter label
	if !key.HasCapiWatchLabel(awsMachinePool.Labels) {
		logger.Info(fmt.Sprintf("AWSMachinePool do not have %s=%s label, ignoring CR", key.ClusterWatchFilterLabel, "capi"))
		// ignoring this CR
		return ctrl.Result{}, nil
	}

	clusterName := key.GetClusterIDFromLabels(awsMachinePool.ObjectMeta)
	logger = logger.WithValues("cluster", clusterName)

	var awsClientGetter *awsclient.AwsClient
	{
		c := awsclient.AWSClientConfig{
			ClusterName: clusterName,
			CtrlClient:  r.Client,
			Log:         logger,
		}
		awsClientGetter, err = awsclient.New(c)
		if err != nil {
			logger.Error(err, "failed to generate awsClientGetter")
			return ctrl.Result{}, err
		}
	}

	awsClientSession, err := awsClientGetter.GetAWSClientSession(ctx)
	if err != nil {
		logger.Error(err, "Failed to get aws client session")
		return ctrl.Result{}, err
	}

	subnetService := subnet.Service{
		AWSMachinePool: awsMachinePool,
		AWSSession:     awsClientSession,
		CtrlClient:     r.Client,
		CidrRange:      r.DefaultCidrRange,
		Logger:         logger,
		SubnetSize:     r.DefaultSubnetSize,
	}

	if awsMachinePool.DeletionTimestamp != nil {
		// delete resource
		err = subnetService.Delete()
		if err != nil {
			return ctrl.Result{}, err
		}

		// remove finalizer from AWSMachinePool
		err := r.Get(ctx, req.NamespacedName, awsMachinePool)
		if err != nil {
			logger.Error(err, "failed to get latest AWSMachinePool CR")
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(awsMachinePool, key.FinalizerName)
		err = r.Update(ctx, awsMachinePool)
		if err != nil {
			logger.Error(err, "failed to remove finalizer from AWSMachinePool")
			return ctrl.Result{}, err
		}
	} else {
		// reconcile CR
		err = subnetService.Reconcile()
		if err != nil {
			return ctrl.Result{}, err
		}

		// add finalizer to AWSMachinePool
		err := r.Get(ctx, req.NamespacedName, awsMachinePool)
		if err != nil {
			logger.Error(err, "failed to get latest AWSMachinePool CR")
			return ctrl.Result{}, err
		}
		controllerutil.AddFinalizer(awsMachinePool, key.FinalizerName)
		err = r.Update(ctx, awsMachinePool)
		if err != nil {
			logger.Error(err, "failed to add finalizer on AWSMachinePool")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{
		Requeue:      true,
		RequeueAfter: time.Minute * 5,
	}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AWSMachinePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&expcapa.AWSMachinePool{}).
		Complete(r)
}
