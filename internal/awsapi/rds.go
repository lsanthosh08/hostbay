package awsapi

import (
	"context"
	"net/url"
	"strings"
)

const rdsVersion = "2014-10-31"

// Database is an RDS instance as the AWS tab lists it. Kind is the Hostbay
// host kind that opens it, or empty when none does. SaveHost is the address
// a saved host should use: an Aurora member's cluster writer endpoint, which
// survives a failover, else the instance's own. NoClusterEndpoint is set on
// a cluster member whose writer endpoint AWS did not return (a role that may
// not list clusters), so SaveHost fell back to the member's own address.
type Database struct {
	ID                string `json:"id"`
	Engine            string `json:"engine"`
	Status            string `json:"status"`
	Address           string `json:"address"`
	Port              int    `json:"port"`
	User              string `json:"user"`
	DBName            string `json:"dbName"`
	Cluster           string `json:"cluster,omitempty"`
	Kind              string `json:"kind"`
	SaveHost          string `json:"saveHost"`
	NoClusterEndpoint bool   `json:"noClusterEndpoint,omitempty"`
}

// Databases lists every RDS instance in the region, following every page.
func (c *Client) Databases(ctx context.Context) ([]Database, error) {
	var out []Database
	clustered := false
	err := pages(func(params url.Values) (string, error) {
		var r struct {
			Items []struct {
				ID      string `xml:"DBInstanceIdentifier"`
				Engine  string `xml:"Engine"`
				Status  string `xml:"DBInstanceStatus"`
				Address string `xml:"Endpoint>Address"`
				Port    int    `xml:"Endpoint>Port"`
				User    string `xml:"MasterUsername"`
				DBName  string `xml:"DBName"`
				Cluster string `xml:"DBClusterIdentifier"`
			} `xml:"DescribeDBInstancesResult>DBInstances>DBInstance"`
			Marker string `xml:"DescribeDBInstancesResult>Marker"`
		}
		if err := c.query(ctx, "rds", rdsVersion, "DescribeDBInstances", params, &r); err != nil {
			return "", err
		}
		for _, d := range r.Items {
			out = append(out, Database{ID: d.ID, Engine: d.Engine, Status: d.Status, Address: d.Address, Port: d.Port,
				User: d.User, DBName: d.DBName, Cluster: d.Cluster, Kind: DatabaseKind(d.Engine), SaveHost: d.Address})
			clustered = clustered || d.Cluster != ""
		}
		return r.Marker, nil
	})
	if err != nil || !clustered {
		return out, err
	}
	// Point cluster members at the writer endpoint. Without the cluster list
	// each keeps its own address, which still works until a failover.
	type cluster struct {
		ID     string `xml:"DBClusterIdentifier"`
		Host   string `xml:"Endpoint"`
		Port   int    `xml:"Port"`
		User   string `xml:"MasterUsername"`
		DBName string `xml:"DatabaseName"`
	}
	clusters := map[string]cluster{}
	_ = pages(func(params url.Values) (string, error) {
		var r struct {
			Items  []cluster `xml:"DescribeDBClustersResult>DBClusters>DBCluster"`
			Marker string    `xml:"DescribeDBClustersResult>Marker"`
		}
		if err := c.query(ctx, "rds", rdsVersion, "DescribeDBClusters", params, &r); err != nil {
			return "", err
		}
		for _, cl := range r.Items {
			clusters[cl.ID] = cl
		}
		return r.Marker, nil
	})
	for i, d := range out {
		if d.Cluster == "" {
			continue
		}
		cl, ok := clusters[d.Cluster]
		if !ok || cl.Host == "" {
			out[i].NoClusterEndpoint = true
			continue
		}
		out[i].SaveHost = cl.Host
		if cl.Port != 0 {
			out[i].Port = cl.Port
		}
		if d.User == "" {
			out[i].User = cl.User
		}
		if d.DBName == "" {
			out[i].DBName = cl.DBName
		}
	}
	return out, nil
}

// pages calls page with each Marker until it returns none.
func pages(page func(url.Values) (string, error)) error {
	marker := ""
	for {
		params := url.Values{"MaxRecords": {"100"}}
		if marker != "" {
			params.Set("Marker", marker)
		}
		next, err := page(params)
		if err != nil || next == "" {
			return err
		}
		marker = next
	}
}

// DatabaseKind is the Hostbay host kind for an RDS engine, or "" when
// Hostbay has none.
func DatabaseKind(engine string) string {
	e := strings.TrimPrefix(engine, "custom-")
	switch {
	case e == "postgres" || e == "aurora-postgresql":
		return "postgres"
	case e == "mysql" || e == "mariadb" || e == "aurora" || e == "aurora-mysql":
		return "mysql"
	case strings.HasPrefix(e, "sqlserver-"):
		return "mssql"
	case strings.HasPrefix(e, "oracle-"):
		return "oracle"
	}
	return ""
}
