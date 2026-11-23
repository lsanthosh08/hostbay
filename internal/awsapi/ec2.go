package awsapi

import (
	"context"
	"errors"
	"net/url"
	"slices"
)

const ec2Version = "2016-11-15"

// Instance is an EC2 instance as the AWS tab lists it. Platform is
// "windows" for a Windows instance and empty otherwise.
type Instance struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	State     string `json:"state"`
	Type      string `json:"type"`
	PrivateIP string `json:"privateIp"`
	PublicIP  string `json:"publicIp"`
	PublicDNS string `json:"publicDns"`
	KeyName   string `json:"keyName"`
	Platform  string `json:"platform"`
}

// Instances lists every EC2 instance in the region, following every page.
func (c *Client) Instances(ctx context.Context) ([]Instance, error) {
	var out []Instance
	token := ""
	for {
		params := url.Values{"MaxResults": {"1000"}}
		if token != "" {
			params.Set("NextToken", token)
		}
		var r struct {
			Reservations []struct {
				Instances []struct {
					ID        string `xml:"instanceId"`
					Type      string `xml:"instanceType"`
					State     string `xml:"instanceState>name"`
					PrivateIP string `xml:"privateIpAddress"`
					PublicIP  string `xml:"ipAddress"`
					PublicDNS string `xml:"dnsName"`
					KeyName   string `xml:"keyName"`
					Platform  string `xml:"platform"`
					Tags      []struct {
						Key   string `xml:"key"`
						Value string `xml:"value"`
					} `xml:"tagSet>item"`
				} `xml:"instancesSet>item"`
			} `xml:"reservationSet>item"`
			NextToken string `xml:"nextToken"`
		}
		if err := c.query(ctx, "ec2", ec2Version, "DescribeInstances", params, &r); err != nil {
			return nil, err
		}
		for _, res := range r.Reservations {
			for _, i := range res.Instances {
				in := Instance{ID: i.ID, State: i.State, Type: i.Type, PrivateIP: i.PrivateIP, PublicIP: i.PublicIP, PublicDNS: i.PublicDNS, KeyName: i.KeyName, Platform: i.Platform}
				for _, t := range i.Tags {
					if t.Key == "Name" {
						in.Name = t.Value
					}
				}
				out = append(out, in)
			}
		}
		if r.NextToken == "" {
			return out, nil
		}
		token = r.NextToken
	}
}

// Regions lists the regions enabled for the account. When AWS refuses the
// call (a role without ec2:DescribeRegions), it returns a built-in list
// with checked false instead, so the tab can still reach other regions. A
// credentials failure is still an error.
func (c *Client) Regions(ctx context.Context) (regions []string, checked bool, err error) {
	var r struct {
		Names []string `xml:"regionInfo>item>regionName"`
	}
	err = c.query(ctx, "ec2", ec2Version, "DescribeRegions", nil, &r)
	var api *APIError
	if errors.As(err, &api) {
		regions = slices.Clone(knownRegions)
		if !slices.Contains(regions, c.region) {
			regions = append(regions, c.region)
		}
		slices.Sort(regions)
		return regions, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	slices.Sort(r.Names)
	return r.Names, true, nil
}

// knownRegions are the commercial regions, for when the account's own list
// cannot be read.
var knownRegions = []string{
	"af-south-1", "ap-east-1", "ap-northeast-1", "ap-northeast-2", "ap-northeast-3", "ap-south-1", "ap-south-2",
	"ap-southeast-1", "ap-southeast-2", "ap-southeast-3", "ap-southeast-4", "ap-southeast-5", "ca-central-1", "ca-west-1",
	"eu-central-1", "eu-central-2", "eu-north-1", "eu-south-1", "eu-south-2", "eu-west-1", "eu-west-2", "eu-west-3",
	"il-central-1", "me-central-1", "me-south-1", "mx-central-1", "sa-east-1", "us-east-1", "us-east-2", "us-west-1", "us-west-2",
}

// InstanceAction starts, stops or reboots one instance.
func (c *Client) InstanceAction(ctx context.Context, id, action string) error {
	api, ok := map[string]string{"start": "StartInstances", "stop": "StopInstances", "reboot": "RebootInstances"}[action]
	if !ok {
		return errors.New("unknown instance action: " + action)
	}
	if !ValidInstanceID(id) {
		return errors.New("not an EC2 instance ID: " + id)
	}
	var r struct{}
	return c.query(ctx, "ec2", ec2Version, api, url.Values{"InstanceId.1": {id}}, &r)
}
