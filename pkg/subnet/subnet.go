package subnet

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	"net"
	capa "sigs.k8s.io/cluster-api-provider-aws/api/v1alpha3"
	"strconv"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/giantswarm/capa-machinepool-subnet-operator/pkg/key"
	"github.com/giantswarm/ipam"
	expcapa "sigs.k8s.io/cluster-api-provider-aws/exp/api/v1alpha3"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

type Service struct {
	AWSSession     client.ConfigProvider
	AWSMachinePool *expcapa.AWSMachinePool
	CtrlClient     ctrlclient.Client
	Logger         logr.Logger

	CidrRange  string
	SubnetSize string
}

func (s *Service) Reconcile() error {
	ec2Client := ec2.New(s.AWSSession)
	ctx := context.TODO()

	azCount := len(s.AWSMachinePool.Spec.AvailabilityZones)

	clusterName := key.GetClusterIDFromLabels(s.AWSMachinePool.ObjectMeta)

	awsCluster, err := key.GetAWSClusterByName(ctx, s.CtrlClient, clusterName)
	if err != nil {
		s.Logger.Error(err, "failed to fetch AWSCluster")
		return err
	}

	var cidrBlock net.IPNet
	// check if cidr is already assigned
	if a, ok := s.AWSMachinePool.GetAnnotations()[key.AnnotationAssignedCIDR]; ok {
		_, c, err := net.ParseCIDR(a)
		if err != nil {
			s.Logger.Error(err,"failed to parse awsMachinePool cidr range")
			return err
		}
		cidrBlock = *c

	} else {
		// no block is assigned to the MP, get a new one
		cidrBlock, err = s.getFreeCidrBlock(ctx, clusterName, awsCluster.Spec.NetworkSpec.VPC.CidrBlock)
		if err != nil {
			s.Logger.Error(err, "failed to get free cidr block for awsMachinePool")
			return err
		}
		// put annotation on the CR
		s.AWSMachinePool.Annotations[key.AnnotationAssignedCIDR] = cidrBlock.String()
		err = s.CtrlClient.Update(ctx, s.AWSMachinePool)
		if err != nil {
			s.Logger.Error(err,"failed to update AWSMachinePool")
			return err
		}
	}

	i := &ec2.DescribeVpcsInput{VpcIds: aws.StringSlice([]string{awsCluster.Spec.NetworkSpec.VPC.ID})}
	vpcs, err := ec2Client.DescribeVpcs(i)
	if err != nil {
		s.Logger.Error(err,"failed to describe VPCs")
		return err
	}
	// if the cidr block is not associated yet we will add it to the VPC
	if !key.IsCidrAlreadyAssociated(cidrBlock.String(), vpcs.Vpcs[0].CidrBlockAssociationSet) {
		i := &ec2.AssociateVpcCidrBlockInput{
			CidrBlock: aws.String(cidrBlock.String()),
			VpcId:     aws.String(awsCluster.Spec.NetworkSpec.VPC.ID),
		}
		_, err = ec2Client.AssociateVpcCidrBlock(i)
		if err != nil {
			s.Logger.Error(err,"failed to associate CIDR block to cluster vpc")
			return err
		}
	}

	// add new subnets to the AWSCluster CR if they are missing
	{
		// check if the subnets are already added
		subnetRanges, err := ipam.Split(cidrBlock, uint(azCount))
		if err != nil {
			s.Logger.Error(err,fmt.Sprintf("failed to split cidr '%s' into %d zones",cidrBlock.String(), azCount))
			return err
		}
		clusterSubnetSpecs := awsCluster.Spec.NetworkSpec.Subnets
		subnetFound := false
		for _, s1 := range clusterSubnetSpecs {
			for _, s2 := range subnetRanges {
				if s1.CidrBlock == s2.String() {
					subnetFound = true
					break
				}
			}
		}

		if !subnetFound {
			// add new subnets to AWSCluster CR
			var newSubnetSpecs []*capa.SubnetSpec
			{
				for i, r := range subnetRanges {
					subnet := &capa.SubnetSpec{
						CidrBlock:        r.String(),
						AvailabilityZone: s.AWSMachinePool.Spec.AvailabilityZones[i],
						IsPublic:         false,
						Tags:             key.SubnetTags(s.AWSMachinePool.Name),
					}

					newSubnetSpecs = append(newSubnetSpecs, subnet)
				}
			}

			clusterSubnetSpecs = append(clusterSubnetSpecs, newSubnetSpecs...)
			awsCluster.Spec.NetworkSpec.Subnets = clusterSubnetSpecs

			err = s.CtrlClient.Update(ctx, awsCluster)
			if err != nil {
				s.Logger.Error(err,"failed to add new subnets to AWSCluster")
				return err
			}
		}
	}

	return nil
}

func (s *Service) Delete() error {
	ec2Client := ec2.New(s.AWSSession)
	ctx := context.TODO()

	if cidr, ok := s.AWSMachinePool.GetAnnotations()[key.AnnotationAssignedCIDR]; ok {
		clusterName := key.GetClusterIDFromLabels(s.AWSMachinePool.ObjectMeta)

		awsCluster, err := key.GetAWSClusterByName(ctx, s.CtrlClient, clusterName)
		if err != nil {
			s.Logger.Error(err,"failed to get AWSCluster CR")
			return err
		}

		i := &ec2.DescribeVpcsInput{VpcIds: aws.StringSlice([]string{awsCluster.Spec.NetworkSpec.VPC.ID})}
		vpcs, err := ec2Client.DescribeVpcs(i)
		if err != nil {
			s.Logger.Error(err,"failed to describe VPCs")
			return err
		}
		var associationID *string
		for _, a := range vpcs.Vpcs[0].CidrBlockAssociationSet {
			if *a.CidrBlock == cidr {
				associationID = a.AssociationId
			}
		}

		if associationID != nil {
			i := &ec2.DisassociateSubnetCidrBlockInput{
				AssociationId: associationID,
			}
			_, err := ec2Client.DisassociateSubnetCidrBlock(i)
			if err != nil {
				s.Logger.Error(err,"failed to disassociate cidr block from cluster VPC")
				return err
			}
		}

		// remove the annotation for cidr reservation
		newAnnotations := s.AWSMachinePool.GetAnnotations()
		delete(newAnnotations, key.AnnotationAssignedCIDR)
		s.AWSMachinePool.Annotations = newAnnotations

		err = s.CtrlClient.Update(ctx, s.AWSMachinePool)
		if err != nil {
			s.Logger.Error(err,"failed to remove cidr block  annotation from AWSMachinePool")
			return err
		}
	}

	return nil
}

func (s *Service) getFreeCidrBlock(ctx context.Context, clusterName string, vpcCidrBlock string) (net.IPNet, error) {
	size, err := strconv.Atoi(s.SubnetSize)
	if err != nil {
		return net.IPNet{}, err
	}

	subnetIPMask := net.CIDRMask(size, 32)
	_, subnetRange, err := net.ParseCIDR(s.CidrRange)

	var usedSubnets []net.IPNet
	{
		// add VPC cidr to already used subnet list
		_, vpc, err := net.ParseCIDR(vpcCidrBlock)
		if err != nil {
			return net.IPNet{}, err
		}
		usedSubnets = append(usedSubnets, *vpc)

		// add ranges used by other aws machine pools
		var awsMachinePoolList *expcapa.AWSMachinePoolList
		err = s.CtrlClient.List(ctx, awsMachinePoolList, ctrlclient.MatchingLabels{key.ClusterNameLabel: clusterName})
		if err != nil {
			return net.IPNet{}, err
		}
		for _, mp := range awsMachinePoolList.Items {
			if a, ok := mp.GetAnnotations()[key.AnnotationAssignedCIDR]; ok {
				_, s, err := net.ParseCIDR(a)
				if err != nil {
					return net.IPNet{}, err
				}
				usedSubnets = append(usedSubnets, *s)
			}
		}

	}

	freeBlock, err := ipam.Free(*subnetRange, subnetIPMask, usedSubnets)
	if err != nil {
		return net.IPNet{}, err
	}

	return freeBlock, nil
}
