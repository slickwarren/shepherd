package etcdsnapshot

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/rancher/norman/types"
	apisV1 "github.com/rancher/rancher/pkg/apis/provisioning.cattle.io/v1"
	rkev1 "github.com/rancher/rancher/pkg/apis/rke.cattle.io/v1"
	"github.com/rancher/shepherd/clients/rancher"
	management "github.com/rancher/shepherd/clients/rancher/generated/management/v3"
	rancherv1 "github.com/rancher/shepherd/clients/rancher/v1"
	"github.com/rancher/shepherd/extensions/clusters"
	"github.com/rancher/shepherd/extensions/defaults"
	"github.com/rancher/shepherd/extensions/defaults/stevetypes"
	"github.com/sirupsen/logrus"
	kwait "k8s.io/apimachinery/pkg/util/wait"
)

const (
	ProvisioningSteveResouceType = "provisioning.cattle.io.cluster"
	fleetNamespace               = "fleet-default"
	localClusterName             = "local"
	active                       = "active"
	readyStatus                  = "Resource is ready"
)

// GetRKE1Snapshots is a helper function to get the existing snapshots for a downstream RKE1 cluster.
func GetRKE1Snapshots(client *rancher.Client, clusterName string) ([]management.EtcdBackup, error) {
	clusterID, err := clusters.GetClusterIDByName(client, clusterName)
	if err != nil {
		return nil, err
	}

	snapshotSteveObjList, err := client.Management.EtcdBackup.ListAll(&types.ListOpts{
		Filters: map[string]interface{}{
			"clusterId": clusterID,
		},
	})
	if err != nil {
		return nil, err
	}

	snapshots := []management.EtcdBackup{}

	for _, snapshot := range snapshotSteveObjList.Data {
		if strings.Contains(snapshot.Name, clusterID) {
			snapshots = append(snapshots, snapshot)
		}
	}

	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].Created > snapshots[j].Created
	})

	return snapshots, nil
}

// GetRKE2K3SSnapshots is a helper function to get the existing snapshots for a downstream RKE2/K3S cluster.
func GetRKE2K3SSnapshots(client *rancher.Client, clusterName string) ([]rancherv1.SteveAPIObject, error) {
	localclusterID, err := clusters.GetClusterIDByName(client, localClusterName)
	if err != nil {
		return nil, err
	}

	steveclient, err := client.Steve.ProxyDownstream(localclusterID)
	if err != nil {
		return nil, err
	}

	snapshotSteveObjList, err := steveclient.SteveType(stevetypes.EtcdSnapshot).List(nil)
	if err != nil {
		return nil, err
	}

	snapshots := []rancherv1.SteveAPIObject{}

	for _, snapshot := range snapshotSteveObjList.Data {
		if strings.Contains(snapshot.ObjectMeta.Name, clusterName) {
			snapshots = append(snapshots, snapshot)
		}
	}

	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].ObjectMeta.CreationTimestamp.Before(&snapshots[j].ObjectMeta.CreationTimestamp)
	})

	return snapshots, nil
}

// CreateRKE1Snapshot is a helper function to create a snapshot on an RKE1 cluster. Returns error if any.
func CreateRKE1Snapshot(client *rancher.Client, clusterName string) error {
	clusterID, err := clusters.GetClusterIDByName(client, clusterName)
	if err != nil {
		return err
	}

	clusterResp, err := client.Management.Cluster.ByID(clusterID)
	if err != nil {
		return err
	}

	logrus.Infof("Creating snapshot...")
	err = client.Management.Cluster.ActionBackupEtcd(clusterResp)
	if err != nil {
		return err
	}

	err = kwait.PollUntilContextTimeout(context.TODO(), 5*time.Second, defaults.FiveMinuteTimeout, true, func(ctx context.Context) (done bool, err error) {
		snapshotSteveObjList, err := client.Management.EtcdBackup.ListAll(&types.ListOpts{
			Filters: map[string]interface{}{
				"clusterId": clusterID,
			},
		})
		if err != nil {
			return false, nil
		}

		for _, snapshot := range snapshotSteveObjList.Data {
			snapshotObj, err := client.Management.EtcdBackup.ByID(snapshot.ID)
			if err != nil {
				return false, nil
			}

			if snapshotObj.State != active {
				return false, nil
			}
		}

		logrus.Infof("All snapshots in the cluster are in an active state!")
		return true, nil
	})
	if err != nil {
		return err
	}

	return nil
}

// CreateRKE2K3SSnapshot is a helper function to create a snapshot on an RKE2 or k3s cluster. Returns error if any.
func CreateRKE2K3SSnapshot(client *rancher.Client, clusterName string) error {
	clusterObject, clusterSteveObject, err := clusters.GetProvisioningClusterByName(client, clusterName, fleetNamespace)
	if err != nil {
		return err
	}

	if clusterObject.Spec.RKEConfig != nil {
		if clusterObject.Spec.RKEConfig.ETCDSnapshotCreate == nil {
			clusterObject.Spec.RKEConfig.ETCDSnapshotCreate = &rkev1.ETCDSnapshotCreate{
				Generation: 1,
			}
		} else {
			clusterObject.Spec.RKEConfig.ETCDSnapshotCreate = &rkev1.ETCDSnapshotCreate{
				Generation: clusterObject.Spec.RKEConfig.ETCDSnapshotCreate.Generation + 1,
			}
		}
	} else {
		clusterObject.Spec.RKEConfig = &apisV1.RKEConfig{
			ETCDSnapshotCreate: &rkev1.ETCDSnapshotCreate{
				Generation: 1,
			},
		}
	}

	logrus.Infof("Creating snapshot...")
	_, err = client.Steve.SteveType(clusters.ProvisioningSteveResourceType).Update(clusterSteveObject, clusterObject)
	if err != nil {
		return err
	}

	err = kwait.PollUntilContextTimeout(context.TODO(), 5*time.Second, defaults.FiveMinuteTimeout, true, func(ctx context.Context) (done bool, err error) {
		snapshotSteveObjList, err := client.Steve.SteveType("rke.cattle.io.etcdsnapshot").List(nil)
		if err != nil {
			return false, nil
		}

		_, clusterSteveObject, err := clusters.GetProvisioningClusterByName(client, clusterName, fleetNamespace)
		if err != nil {
			return false, nil
		}

		for _, snapshot := range snapshotSteveObjList.Data {
			snapshotObj, err := client.Steve.SteveType("rke.cattle.io.etcdsnapshot").ByID(snapshot.ID)
			if err != nil {
				return false, nil
			}

			if snapshotObj.ObjectMeta.State.Name == active && clusterSteveObject.ObjectMeta.State.Name == active {
				logrus.Infof("All snapshots in the cluster are in an active state!")
				return true, nil
			}
		}

		return false, nil
	})
	if err != nil {
		return err
	}

	return nil
}

// RestoreRKE1Snapshot is a helper function to restore a snapshot on an RKE1 cluster. Returns error if any.
func RestoreRKE1Snapshot(client *rancher.Client, clusterName string, snapshotRestore *management.RestoreFromEtcdBackupInput) error {
	clusterID, err := clusters.GetClusterIDByName(client, clusterName)
	if err != nil {
		return err
	}

	cluster, err := client.Management.Cluster.ByID(clusterID)
	if err != nil {
		return err
	}

	logrus.Infof("Restoring snapshot: %v", snapshotRestore.EtcdBackupID)
	err = client.Management.Cluster.ActionRestoreFromEtcdBackup(cluster, snapshotRestore)
	if err != nil {
		return err
	}

	err = kwait.PollUntilContextTimeout(context.TODO(), 1*time.Second, defaults.OneMinuteTimeout, true, func(ctx context.Context) (done bool, err error) {
		clusterResp, err := client.Management.Cluster.ByID(cluster.ID)
		if err != nil {
			return false, nil
		}

		if clusterResp.State != active {
			return true, nil
		}

		return false, nil
	})
	if err != nil {
		return err
	}

	// Timeout is specifically set to 30 minutes due to expected behavior with RKE1 nodes.
	err = kwait.PollUntilContextTimeout(context.TODO(), 5*time.Second, defaults.ThirtyMinuteTimeout, true, func(ctx context.Context) (done bool, err error) {
		clusterResp, err := client.Management.Cluster.ByID(cluster.ID)
		if err != nil {
			return false, nil
		}

		if clusterResp.State == active {
			return true, nil
		}

		return false, nil
	})
	if err != nil {
		return err
	}

	return nil
}

// RestoreRKE2K3SSnapshot is a helper function to restore a snapshot on an RKE2 or k3s cluster. Returns error if any.
func RestoreRKE2K3SSnapshot(client *rancher.Client, snapshotRestore *rkev1.ETCDSnapshotRestore, clusterName string) error {
	clusterObject, existingSteveAPIObject, err := clusters.GetProvisioningClusterByName(client, clusterName, fleetNamespace)
	if err != nil {
		return err
	}

	clusterObject.Spec.RKEConfig.ETCDSnapshotRestore = snapshotRestore

	logrus.Infof("Restoring snapshot: %v", snapshotRestore.Name)
	updatedCluster, err := client.Steve.SteveType(ProvisioningSteveResouceType).Update(existingSteveAPIObject, clusterObject)
	if err != nil {
		return err
	}

	err = kwait.PollUntilContextTimeout(context.TODO(), 500*time.Millisecond, defaults.OneMinuteTimeout, true, func(ctx context.Context) (done bool, err error) {
		clusterResp, err := client.Steve.SteveType(ProvisioningSteveResouceType).ByID(updatedCluster.ID)
		if err != nil {
			return false, err
		}

		clusterStatus := &apisV1.ClusterStatus{}
		err = rancherv1.ConvertToK8sType(clusterResp.Status, clusterStatus)
		if err != nil {
			return false, err
		}

		if clusterResp.ObjectMeta.State.Name != active {
			return true, nil
		}

		return false, nil
	})
	if err != nil {
		return err
	}

	err = kwait.PollUntilContextTimeout(context.TODO(), 500*time.Millisecond, defaults.FifteenMinuteTimeout, true, func(ctx context.Context) (done bool, err error) {
		clusterResp, err := client.Steve.SteveType(ProvisioningSteveResouceType).ByID(updatedCluster.ID)
		if err != nil {
			return false, err
		}

		clusterStatus := &apisV1.ClusterStatus{}
		err = rancherv1.ConvertToK8sType(clusterResp.Status, clusterStatus)
		if err != nil {
			return false, err
		}

		if clusterResp.ObjectMeta.State.Name == active {
			return true, nil
		}

		return false, nil
	})
	if err != nil {
		return err
	}

	return nil
}
