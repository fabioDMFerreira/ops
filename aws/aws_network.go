package aws

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/nanovms/ops/lepton"
	"github.com/nanovms/ops/network"
	"github.com/nanovms/ops/types"
)

// GetSecurityGroup checks whether the configuration security group exists and has the configuration VPC assigned
func (p *AWS) GetSecurityGroup(ctx *lepton.Context, svc *ec2.EC2, vpc *ec2.Vpc) (sg *ec2.SecurityGroup, err error) {
	sgName := ctx.Config().CloudConfig.SecurityGroup

	input := &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("group-name"),
				Values: aws.StringSlice([]string{sgName}),
			},
		},
	}

	var result *ec2.DescribeSecurityGroupsOutput

	result, err = svc.DescribeSecurityGroups(input)
	if err != nil {
		return
	} else if len(result.SecurityGroups) == 0 {
		input := &ec2.DescribeSecurityGroupsInput{
			GroupIds: []*string{
				aws.String(sgName),
			},
		}

		result, err = svc.DescribeSecurityGroups(input)
		if err != nil {
			err = fmt.Errorf("get security group with id '%s': %s", sg, err.Error())
			return
		}
	}

	if len(result.SecurityGroups) == 1 && *result.SecurityGroups[0].VpcId != *vpc.VpcId {
		err = fmt.Errorf("vpc mismatch: expected '%s' to have vpc '%s', got '%s'", sgName, *vpc.VpcId, *result.SecurityGroups[0].VpcId)
		return
	} else if len(result.SecurityGroups) == 0 {
		err = fmt.Errorf("security group '%s' not found", sgName)
		return
	}

	sg = result.SecurityGroups[0]

	return
}

// GetSubnet returns a subnet with the context subnet name or the default subnet of vpc passed by argument
func (p *AWS) GetSubnet(ctx *lepton.Context, svc *ec2.EC2, vpcID string) (*ec2.Subnet, error) {
	subnetName := ctx.Config().CloudConfig.Subnet
	var filters []*ec2.Filter
	var result *ec2.DescribeSubnetsOutput
	var err error

	filters = append(filters, &ec2.Filter{Name: aws.String("vpc-id"), Values: aws.StringSlice([]string{vpcID})})

	if subnetName != "" {
		result, err = svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
			Filters: append(filters, &ec2.Filter{Name: aws.String("tag:Name"), Values: aws.StringSlice([]string{subnetName})}),
		})
		if err != nil {
			err = fmt.Errorf("unable to describe subnets, %v", err)
			return nil, err
		} else if len(result.Subnets) == 0 {
			result, err = svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
				SubnetIds: aws.StringSlice([]string{subnetName}),
				Filters:   filters,
			})
			if err != nil {
				return nil, err
			}
		}
	} else {
		input := &ec2.DescribeSubnetsInput{
			Filters: filters,
		}

		result, err = svc.DescribeSubnets(input)
		if err != nil {
			err = fmt.Errorf("unable to describe subnets, %v", err)
			return nil, err
		}
	}

	if len(result.Subnets) == 0 && subnetName != "" {
		return nil, fmt.Errorf("no subnets with name '%v' found to associate security group with", subnetName)
	} else if len(result.Subnets) == 0 {
		return nil, errors.New("no subnets found to associate security group with")
	}

	if subnetName != "" {
		for _, subnet := range result.Subnets {
			if *subnet.DefaultForAz {
				return subnet, nil
			}
		}
	}

	return result.Subnets[0], nil
}

// GetVPC returns a vpc with the context vpc name or the default vpc
func (p *AWS) GetVPC(ctx *lepton.Context, svc *ec2.EC2) (*ec2.Vpc, error) {
	vpcName := ctx.Config().CloudConfig.VPC

	var vpc *ec2.Vpc
	var input *ec2.DescribeVpcsInput
	var result *ec2.DescribeVpcsOutput
	var err error

	if vpcName != "" {
		ctx.Logger().Debug("getting vpcs filtered by name %s", vpcName)
		var filters []*ec2.Filter

		filters = append(filters, &ec2.Filter{Name: aws.String("tag:Name"), Values: aws.StringSlice([]string{vpcName})})
		input = &ec2.DescribeVpcsInput{
			Filters: filters,
		}

		result, err = svc.DescribeVpcs(input)
		if err != nil {
			return nil, fmt.Errorf("unable to describe VPCs, %v", err)
		}

		if len(result.Vpcs) == 0 {
			r, _ := regexp.Compile("^vpc-.*")

			match := r.FindStringSubmatch(vpcName)

			if len(match) == 0 {
				return nil, nil
			}

			ctx.Logger().Debug("no vpcs with name %s found", vpcName)
			ctx.Logger().Debug("getting vpcs filtered by id %s", vpcName)
			input = &ec2.DescribeVpcsInput{
				VpcIds: aws.StringSlice([]string{vpcName}),
			}
			result, err = svc.DescribeVpcs(input)
			if err != nil {
				return nil, fmt.Errorf("unable to describe VPCs, %v", err)
			}
		}

		ctx.Logger().Debug("found %d vpcs that match the criteria %s", len(result.Vpcs), vpcName)
		vpc = result.Vpcs[0]
	} else {
		ctx.Logger().Debug("no vpc name specified")
		ctx.Logger().Debug("getting all vpcs")
		result, err = svc.DescribeVpcs(input)
		if err != nil {
			return nil, fmt.Errorf("unable to describe VPCs, %v", err)
		}

		// select default vpc
		for i, s := range result.Vpcs {
			isDefault := *s.IsDefault
			if isDefault {
				ctx.Logger().Debug("picking default vpc")
				vpc = result.Vpcs[i]
			}
		}

		// if there is no default VPC select the first vpc of the list
		if vpc == nil && len(result.Vpcs) != 0 {
			ctx.Logger().Debug("no default vpc found")
			vpc = result.Vpcs[0]
			ctx.Logger().Debug("picking vpc %+v", vpc)
		}
	}

	if vpc == nil {
		return nil, errors.New("no VPCs found")
	}

	return vpc, nil
}

func (p AWS) buildFirewallRule(protocol string, port string) *ec2.IpPermission {
	fromPort := port
	toPort := port

	if strings.Contains(port, "-") {
		rangeParts := strings.Split(port, "-")
		fromPort = rangeParts[0]
		toPort = rangeParts[1]
	}

	fromPortInt, err := strconv.Atoi(fromPort)
	if err != nil {
		panic(err)
	}

	toPortInt, err := strconv.Atoi(toPort)
	if err != nil {
		panic(err)
	}

	var ec2Permission = new(ec2.IpPermission)
	ec2Permission.SetIpProtocol(protocol)
	ec2Permission.SetFromPort(int64(fromPortInt))
	ec2Permission.SetToPort(int64(toPortInt))
	ec2Permission.SetIpRanges([]*ec2.IpRange{
		{CidrIp: aws.String("0.0.0.0/0")},
	})

	return ec2Permission
}

// CreateSG - Create security group
func (p *AWS) CreateSG(ctx *lepton.Context, svc *ec2.EC2, imgName string, vpcID string) (sg *ec2.SecurityGroup, err error) {
	t := time.Now().UnixNano()
	s := strconv.FormatInt(t, 10)

	sgName := imgName + s

	createRes, err := svc.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(sgName),
		Description: aws.String("security group for " + imgName),
		VpcId:       aws.String(vpcID),
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case "InvalidVpcID.NotFound":
				errstr := fmt.Sprintf("Unable to find VPC with ID %q.", vpcID)
				err = errors.New(errstr)
				return
			case "InvalidGroup.Duplicate":
				errstr := fmt.Sprintf("Security group %q already exists.", imgName)
				err = errors.New(errstr)
				return
			}
		}
		errstr := fmt.Sprintf("Unable to create security group %q, %v", imgName, err)
		err = errors.New(errstr)
		return
	}
	fmt.Printf("Created security group %s with VPC %s.\n",
		aws.StringValue(createRes.GroupId), vpcID)

	var ec2Permissions []*ec2.IpPermission

	for _, port := range ctx.Config().RunConfig.Ports {
		rule := p.buildFirewallRule("tcp", port)
		ec2Permissions = append(ec2Permissions, rule)
	}

	for _, port := range ctx.Config().RunConfig.UDPPorts {
		rule := p.buildFirewallRule("udp", port)
		ec2Permissions = append(ec2Permissions, rule)
	}

	// maybe have these ports specified from config.json in near future
	if len(ec2Permissions) != 0 {
		_, err = svc.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
			GroupId:       createRes.GroupId,
			IpPermissions: ec2Permissions,
		})
		if err != nil {
			errstr := fmt.Sprintf("Unable to set security group %q ingress, %v", imgName, err)
			err = errors.New(errstr)
			return
		}
	}

	result, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		GroupIds: aws.StringSlice([]string{*createRes.GroupId}),
	})
	if err != nil {
		return
	} else if len(result.SecurityGroups) == 0 {
		err = errors.New("failed creating security group")
	}

	sg = result.SecurityGroups[0]

	return
}

// CreateVPC creates a virtual network
func (p *AWS) CreateVPC(ctx *lepton.Context, svc *ec2.EC2) (vpc *ec2.Vpc, err error) {
	vnetName := ctx.Config().CloudConfig.VPC

	if vnetName == "" {
		err = errors.New("specify vpc name")
		return
	}

	tags, _ := buildAwsTags([]types.Tag{}, vnetName)

	vpcs, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{})
	if err != nil {
		return
	}

	cidrBlocks := []string{}

	for _, v := range vpcs.Vpcs {
		cidrBlocks = append(cidrBlocks, *v.CidrBlock)
	}

	createInput := &ec2.CreateVpcInput{
		CidrBlock: aws.String(network.AllocateNewCidrBlock(cidrBlocks)),
		TagSpecifications: []*ec2.TagSpecification{
			{Tags: tags, ResourceType: aws.String("vpc")},
		},
	}

	if ctx.Config().CloudConfig.EnableIPv6 {
		createInput.SetAmazonProvidedIpv6CidrBlock(true)
	}

	_, err = svc.CreateVpc(createInput)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			default:
				err = errors.New(aerr.Error())
			}
		} else {
			err = errors.New(err.Error())
		}
		return
	}

	vpc, err = p.GetVPC(ctx, svc)

	if err == nil {
		tags, _ = buildAwsTags([]types.Tag{}, ctx.Config().CloudConfig.Subnet)

		_, err = svc.CreateSubnet(&ec2.CreateSubnetInput{
			VpcId:     vpc.VpcId,
			CidrBlock: vpc.CidrBlock,
			TagSpecifications: []*ec2.TagSpecification{
				{Tags: tags, ResourceType: aws.String("subnet")},
			},
		})
	}

	return
}
