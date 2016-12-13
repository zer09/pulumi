// Copyright 2016 Marapongo, Inc. All rights reserved.

import * as mu from 'mu';
import * as cloudformation from '../cloudformation';

// A Virtual Private Cloud (VPC) with a specified CIDR block.
// @name: aws/ec2/vpc
// @website: http://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/aws-resource-ec2-vpc.html
export class VPC extends cloudformation.Resource {
    constructor(ctx: mu.Context, args: VPCArgs) {
        cloudformation.expandTags(args);
        super(ctx, {
            resource: "AWS::EC2::VPC",
            properties: args,
        });
    }
}

export interface VPCArgs extends cloudformation.TagArgs {
    // The CIDR block you want the VPC to cover.  For example, "10.0.0.0/16".
    readonly cidrBlock: string;
    // The allowed tenancy of instances launched into the VPC.  "default" indicates that instances can be launched with
    // any tenancy, while "dedicated" indicates that any instance launched into the VPC automatically has dedicated
    // tenancy, unless you launch it with the default tenancy.
    readonly instanceTenancy?: VPCInstanceTenancy;
    // Specifies whether DNS resolution is supported for the VPC.  If true, the Amazon DNS server resolves DNS hostnames
    // for your instances to their corresponding IP addresses; otherwise, it does not.  By default, the value is true. 
    enableDnsSupport?: boolean;
    // Specifies whether the instances launched in the VPC get DNS hostnames.  If this attribute is true, instances in
    // the VPC get DNS hostnames; otherwise, they do not.  You can only set enableDnsHostnames to true if you also set
    // the enableDnsSupport property to true.  By default, the value is set to false.
    enableDnsHostnames?: boolean;
    // An optional name for this resource.
    name?: string;
    // An arbitrary set of tags (key-value pairs) for this resource.
    tags?: cloudformation.Tags;
}

export type VPCInstanceTenancy = "default" | "dedicated";
