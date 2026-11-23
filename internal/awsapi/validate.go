package awsapi

import "regexp"

var (
	regionRE   = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d{1,2}$`)
	instanceRE = regexp.MustCompile(`^i-[0-9a-f]{8,17}$`)
)

// ValidRegion reports a well-formed region name such as us-east-1.
func ValidRegion(r string) bool { return regionRE.MatchString(r) }

// ValidInstanceID reports a well-formed EC2 instance ID.
func ValidInstanceID(id string) bool { return instanceRE.MatchString(id) }
