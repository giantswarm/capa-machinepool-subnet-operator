package key

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/giantswarm/kubelock"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	capa "sigs.k8s.io/cluster-api-provider-aws/api/v1alpha3"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ClusterNameLabel        = "cluster.x-k8s.io/cluster-name"
	ClusterWatchFilterLabel = "cluster.x-k8s.io/watch-filter"

	FinalizerName = "capa-machinepool-subnet-operator.finalizers.giantswarm.io"

	AnnotationAssignedCIDR = "machinepool.aws.giantswarm.io/reserved-cidr"

	MachinePoolSubnetTag = "sigs.k8s.io/cluster-api-provider-aws/machinepool"
)

func GetClusterIDFromLabels(t v1.ObjectMeta) string {
	return t.GetLabels()[ClusterNameLabel]
}

func GetAWSClusterByName(ctx context.Context, ctrlClient client.Client, clusterName string) (*capa.AWSCluster, error) {
	awsClusterList := &capa.AWSClusterList{}

	if err := ctrlClient.List(ctx,
		awsClusterList,
		client.MatchingLabels{ClusterNameLabel: clusterName},
	); err != nil {
		return nil, err
	}

	if len(awsClusterList.Items) != 1 {
		return nil, fmt.Errorf("expected 1 AWSCluster but found %d", len(awsClusterList.Items))
	}

	return &awsClusterList.Items[0], nil
}

func HasCapiWatchLabel(labels map[string]string) bool {
	value, ok := labels[ClusterWatchFilterLabel]
	if ok {
		if value == "capi" {
			return true
		}
	}
	return false
}

func SubnetTags(nodepoolName string) capa.Tags {
	var tags capa.Tags
	tags[MachinePoolSubnetTag] = nodepoolName
	return tags
}

func IsCidrAlreadyAssociated(cidr string, list []*ec2.VpcCidrBlockAssociation) bool {
	for _, a := range list {
		if cidr == *a.CidrBlock {
			return true
		}
	}
	return false
}

func GetLock(clusterName string) (kubelock.NamespaceableLock, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	c := kubelock.Config{
		DynClient: dynClient,
		GVR: schema.GroupVersionResource{
			Resource: "namespace",
			Group:    v1.SchemeGroupVersion.Group,
			Version:  v1.SchemeGroupVersion.Version,
		},
	}
	kl, err := kubelock.New(c)
	if err != nil {

		return nil, err
	}
	lock := kl.Lock(clusterName)

	return lock, nil
}
