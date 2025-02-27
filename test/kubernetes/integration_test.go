// +build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cloudscale-ch/cloudscale-go-sdk"
	"github.com/cloudscale-ch/csi-cloudscale/driver"
	"github.com/stretchr/testify/assert"
	"golang.org/x/oauth2"
	"k8s.io/client-go/rest"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	// namespace defines the namespace the resources will be created for the CSI tests
	namespace = "csi-test"

	// the luks container has an overhead of 2 MB; the filesystem size is reduced by this much
	luksOverhead = 2 * driver.MB
)

type TestPodVolume struct {
	ClaimName    string
	SizeGB       int
	StorageClass string
	LuksKey      string
	Block        bool
}

type TestPodDescriptor struct {
	Kind    string
	Name    string
	Volumes []TestPodVolume
}

type DiskInfo struct {
	PVCName        string `json:"pvcName"`
	PVCVolumeMode  string `json:"pvcVolumeMode"`
	DeviceName     string `json:"deviceName"`
	DeviceSize     int    `json:"deviceSize"`
	Filesystem     string `json:"filesystem"`
	FilesystemUUID string `json:"filesystemUUID"`
	FilesystemSize int    `json:"filesystemSize"`
	DeviceSource   string `json:"deviceSource"`
	Luks           string `json:"luks,omitempty"`
	Cipher         string `json:"cipher,omitempty"`
	Keysize        int    `json:"keysize,omitempty"`
}

var (
	client           kubernetes.Interface
	config           *rest.Config
	cloudscaleClient *cloudscale.Client
)

func TestMain(m *testing.M) {
	if err := setup(); err != nil {
		log.Fatalln(err)
	}

	// run the tests, don't call any defer yet as it'll fail due `os.Exit()
	exitStatus := m.Run()

	if err := teardown(); err != nil {
		// don't call log.Fatalln() as we exit with `m.Run()`'s exit status
		log.Println(err)
	}

	os.Exit(exitStatus)
}

func TestNode_Zone_Annotation(t *testing.T) {
	labelSelector := "node-role.kubernetes.io/worker=true"
	nodes, err := client.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	assert.NoError(t, err)

	if !(len(nodes.Items) > 0) {
		t.Skipf("Could not find at least one node with label %s", labelSelector)
		return
	}

	for _, node := range nodes.Items {
		assert.Contains(t, []string{"rma1", "lpg1"}, node.Labels["csi.cloudscale.ch/zone"])
	}
}

func TestPod_Single_SSD_Volume(t *testing.T) {
	podDescriptor := TestPodDescriptor{
		Kind: "Pod",
		Name: pseudoUuid(),
		Volumes: []TestPodVolume{
			{
				ClaimName:    "csi-pod-ssd-pvc",
				SizeGB:       5,
				StorageClass: "cloudscale-volume-ssd",
			},
		},
	}

	// submit the pod and the pvc
	pod := makeKubernetesPod(t, podDescriptor)
	pvcs := makeKubernetesPVCs(t, podDescriptor)
	assert.Equal(t, 1, len(pvcs))

	// wait for the pod to be running and verify that the pvc is bound
	waitForPod(t, client, pod.Name)
	pvc := getPVC(t, client, pvcs[0].Name)
	assert.Equal(t, v1.ClaimBound, pvc.Status.Phase)

	// load the volume from the cloudscale.ch api and verify that it
	// has the requested size and volume type
	volume := getCloudscaleVolume(t, pvc.Spec.VolumeName)
	assert.Equal(t, 5, volume.SizeGB)
	assert.Equal(t, "ssd", volume.Type)

	// verify that our disk is not luks-encrypted, formatted with ext4 and 5 GB big
	disk, err := getVolumeInfo(t, pod, pvc.Spec.VolumeName)
	assert.NoError(t, err)
	assert.Equal(t, "", disk.Luks)
	assert.Equal(t, "Filesystem", disk.PVCVolumeMode)
	assert.Equal(t, "ext4", disk.Filesystem)
	assert.Equal(t, 5*driver.GB, disk.DeviceSize)
	assert.Equal(t, 5*driver.GB, disk.FilesystemSize)

	// delete the pod and the pvcs and wait until the volume was deleted from
	// the cloudscale.ch account; this check is necessary to test that the
	// csi-plugin properly deletes the volume from cloudscale.ch
	cleanup(t, podDescriptor)
	waitCloudscaleVolumeDeleted(t, pvc.Spec.VolumeName)
}

func TestPod_Single_SSD_Raw_Volume(t *testing.T) {
	podDescriptor := TestPodDescriptor{
		Kind: "Pod",
		Name: pseudoUuid(),
		Volumes: []TestPodVolume{
			{
				ClaimName:    "csi-pod-ssd-pvc",
				SizeGB:       5,
				StorageClass: "cloudscale-volume-ssd",
				Block:        true,
			},
		},
	}

	// submit the pod and the pvc
	pod := makeKubernetesPod(t, podDescriptor)
	pvcs := makeKubernetesPVCs(t, podDescriptor)
	assert.Equal(t, 1, len(pvcs))

	// wait for the pod to be running and verify that the pvc is bound
	waitForPod(t, client, pod.Name)
	pvc := getPVC(t, client, pvcs[0].Name)
	assert.Equal(t, v1.ClaimBound, pvc.Status.Phase)

	// load the volume from the cloudscale.ch api and verify that it
	// has the requested size and volume type
	volume := getCloudscaleVolume(t, pvc.Spec.VolumeName)
	assert.Equal(t, 5, volume.SizeGB)
	assert.Equal(t, "ssd", volume.Type)

	// verify that our disk is not luks-encrypted, formatted with ext4 and 5 GB big
	disk, err := getVolumeInfo(t, pod, pvc.Spec.VolumeName)
	assert.NoError(t, err)
	assert.Equal(t, "Block", disk.PVCVolumeMode)
	assert.Equal(t, "", disk.Luks)
	assert.Equal(t, "", disk.Filesystem)
	assert.Equal(t, 5*driver.GB, disk.DeviceSize)
	assert.Equal(t, -1, disk.FilesystemSize)

	// delete the pod and the pvcs and wait until the volume was deleted from
	// the cloudscale.ch account; this check is necessary to test that the
	// csi-plugin properly deletes the volume from cloudscale.ch
	cleanup(t, podDescriptor)
	waitCloudscaleVolumeDeleted(t, pvc.Spec.VolumeName)
}

func TestPod_Single_Bulk_Volume(t *testing.T) {
	podDescriptor := TestPodDescriptor{
		Kind: "Pod",
		Name: pseudoUuid(),
		Volumes: []TestPodVolume{
			{
				ClaimName:    "csi-pod-bulk-pvc",
				SizeGB:       100,
				StorageClass: "cloudscale-volume-bulk",
			},
		},
	}

	// submit the pod and the pvc
	pod := makeKubernetesPod(t, podDescriptor)
	pvcs := makeKubernetesPVCs(t, podDescriptor)
	assert.Equal(t, 1, len(pvcs))

	// wait for the pod to be running and verify that the pvc is bound
	waitForPod(t, client, pod.Name)
	pvc := getPVC(t, client, pvcs[0].Name)
	assert.Equal(t, v1.ClaimBound, pvc.Status.Phase)

	// load the volume from the cloudscale.ch api and verify that it
	// has the requested size and volume type
	volume := getCloudscaleVolume(t, pvc.Spec.VolumeName)
	assert.Equal(t, 100, volume.SizeGB)
	assert.Equal(t, "bulk", volume.Type)

	// verify that our disk is not luks-encrypted, formatted with ext4 and 5 GB big
	disk, err := getVolumeInfo(t, pod, pvc.Spec.VolumeName)
	assert.NoError(t, err)
	assert.Equal(t, "", disk.Luks)
	assert.Equal(t, "Filesystem", disk.PVCVolumeMode)
	assert.Equal(t, "ext4", disk.Filesystem)
	assert.Equal(t, 100*driver.GB, disk.DeviceSize)
	assert.Equal(t, 100*driver.GB, disk.FilesystemSize)

	// delete the pod and the pvcs and wait until the volume was deleted from
	// the cloudscale.ch account
	cleanup(t, podDescriptor)
	waitCloudscaleVolumeDeleted(t, pvc.Spec.VolumeName)
}

func TestPod_Single_Bulk_Raw_Volume(t *testing.T) {
	podDescriptor := TestPodDescriptor{
		Kind: "Pod",
		Name: pseudoUuid(),
		Volumes: []TestPodVolume{
			{
				ClaimName:    "csi-pod-bulk-pvc",
				SizeGB:       100,
				StorageClass: "cloudscale-volume-bulk",
				Block:        true,
			},
		},
	}

	// submit the pod and the pvc
	pod := makeKubernetesPod(t, podDescriptor)
	pvcs := makeKubernetesPVCs(t, podDescriptor)
	assert.Equal(t, 1, len(pvcs))

	// wait for the pod to be running and verify that the pvc is bound
	waitForPod(t, client, pod.Name)
	pvc := getPVC(t, client, pvcs[0].Name)
	assert.Equal(t, v1.ClaimBound, pvc.Status.Phase)

	// load the volume from the cloudscale.ch api and verify that it
	// has the requested size and volume type
	volume := getCloudscaleVolume(t, pvc.Spec.VolumeName)
	assert.Equal(t, 100, volume.SizeGB)
	assert.Equal(t, "bulk", volume.Type)

	// verify that our disk is not luks-encrypted, formatted with ext4 and 5 GB big
	disk, err := getVolumeInfo(t, pod, pvc.Spec.VolumeName)
	assert.NoError(t, err)
	assert.Equal(t, "", disk.Luks)
	assert.Equal(t, "Block", disk.PVCVolumeMode)
	assert.Equal(t, "", disk.Filesystem)
	assert.Equal(t, 100*driver.GB, disk.DeviceSize)
	assert.Equal(t, -1, disk.FilesystemSize)

	// delete the pod and the pvcs and wait until the volume was deleted from
	// the cloudscale.ch account
	cleanup(t, podDescriptor)
	waitCloudscaleVolumeDeleted(t, pvc.Spec.VolumeName)
}

func TestDeployment_Single_SSD_Volume(t *testing.T) {
	podDescriptor := TestPodDescriptor{
		Kind: "Deployment",
		Name: pseudoUuid(),
		Volumes: []TestPodVolume{
			{
				ClaimName:    "csi-pod-pvc-0",
				SizeGB:       5,
				StorageClass: "cloudscale-volume-ssd",
			},
		},
	}

	deployment := makeKubernetesDeployment(t, podDescriptor)
	pvcs := makeKubernetesPVCs(t, podDescriptor)

	// Give it a few seconds to create the pod
	time.Sleep(10 * time.Second)

	// get pod associated with the deployment
	selector, err := appSelector(deployment.Name)
	assert.NoError(t, err)

	pods, err := client.CoreV1().Pods(namespace).
		List(context.Background(), metav1.ListOptions{LabelSelector: selector.String()})

	assert.NoError(t, err)
	assert.Equal(t, 1, len(pods.Items))
	pod := pods.Items[0]

	waitForPod(t, client, pod.Name)
	pvc := getPVC(t, client, pvcs[0].Name)
	assert.Equal(t, v1.ClaimBound, pvc.Status.Phase)

	volume := getCloudscaleVolume(t, pvc.Spec.VolumeName)
	assert.Equal(t, 5, volume.SizeGB)
	assert.Equal(t, "ssd", volume.Type)

	// delete the pod and the pvcs and wait until the volume was deleted from
	// the cloudscale.ch account
	cleanup(t, podDescriptor)
	waitCloudscaleVolumeDeleted(t, pvc.Spec.VolumeName)
}

func TestPod_Multi_SSD_Volume(t *testing.T) {
	podDescriptor := TestPodDescriptor{
		Kind: "Pod",
		Name: pseudoUuid(),
		Volumes: []TestPodVolume{
			{
				ClaimName:    "csi-pod-pvc-1",
				SizeGB:       5,
				StorageClass: "cloudscale-volume-ssd",
			},
			{
				ClaimName:    "csi-pod-pvc-2",
				SizeGB:       5,
				StorageClass: "cloudscale-volume-ssd",
			},
		},
	}

	pod := makeKubernetesPod(t, podDescriptor)
	pvcs := makeKubernetesPVCs(t, podDescriptor)
	assert.Equal(t, 2, len(pvcs))

	loadedPVCs := make([]*v1.PersistentVolumeClaim, 0)
	waitForPod(t, client, pod.Name)
	for _, requestedPVC := range pvcs {
		pvc := getPVC(t, client, requestedPVC.Name)
		loadedPVCs = append(loadedPVCs, pvc)
		assert.Equal(t, v1.ClaimBound, pvc.Status.Phase)
		volume := getCloudscaleVolume(t, pvc.Spec.VolumeName)
		assert.Equal(t, 5, volume.SizeGB)
		assert.Equal(t, "ssd", volume.Type)
	}

	// delete the pod and the pvcs and wait until the volume was deleted from
	// the cloudscale.ch account
	cleanup(t, podDescriptor)
	for _, pvc := range loadedPVCs {
		waitCloudscaleVolumeDeleted(t, pvc.Spec.VolumeName)
	}
}

func TestPod_Multiple_Volumes(t *testing.T) {
	podDescriptor := TestPodDescriptor{
		Kind: "Pod",
		Name: pseudoUuid(),
		Volumes: []TestPodVolume{
			{
				ClaimName:    "csi-pod-multi-pvc-1",
				SizeGB:       5,
				StorageClass: "cloudscale-volume-ssd",
			},
			{
				ClaimName:    "csi-pod-multi-pvc-2",
				SizeGB:       100,
				StorageClass: "cloudscale-volume-bulk",
			},
			{
				ClaimName:    "csi-pod-multi-pvc-3",
				SizeGB:       5,
				StorageClass: "cloudscale-volume-ssd",
			},
			{
				ClaimName:    "csi-pod-multi-pvc-4",
				SizeGB:       100,
				StorageClass: "cloudscale-volume-bulk",
			},
		},
	}

	pod := makeKubernetesPod(t, podDescriptor)
	pvcs := makeKubernetesPVCs(t, podDescriptor)
	assert.Equal(t, 4, len(pvcs))

	loadedPVCs := make([]*v1.PersistentVolumeClaim, 0)
	waitForPod(t, client, pod.Name)
	for _, requestedPVC := range pvcs {
		pvc := getPVC(t, client, requestedPVC.Name)
		loadedPVCs = append(loadedPVCs, pvc)
		assert.Equal(t, v1.ClaimBound, pvc.Status.Phase)
		volume := getCloudscaleVolume(t, pvc.Spec.VolumeName)
		if *pvc.Spec.StorageClassName == "cloudscale-volume-bulk" {
			assert.Equal(t, "bulk", volume.Type)
			assert.Equal(t, 100, volume.SizeGB)
		} else {
			assert.Equal(t, "ssd", volume.Type)
			assert.Equal(t, 5, volume.SizeGB)
		}
	}
	// delete the pod and the pvcs and wait until the volume was deleted from
	// the cloudscale.ch account
	cleanup(t, podDescriptor)
	for _, pvc := range loadedPVCs {
		waitCloudscaleVolumeDeleted(t, pvc.Spec.VolumeName)
	}
}

func TestPod_Single_SSD_Luks_Volume(t *testing.T) {
	podDescriptor := TestPodDescriptor{
		Kind: "Pod",
		Name: pseudoUuid(),
		Volumes: []TestPodVolume{
			{
				ClaimName:    "csi-pod-ssd-luks-pvc",
				SizeGB:       5,
				StorageClass: "cloudscale-volume-ssd-luks",
				LuksKey:      "secret",
			},
		},
	}

	// submit the pod and the pvc
	pod := makeKubernetesPod(t, podDescriptor)
	pvcs := makeKubernetesPVCs(t, podDescriptor)
	assert.Equal(t, 1, len(pvcs))

	// wait for the pod to be running and verify that the pvc is bound
	waitForPod(t, client, pod.Name)
	pvc := getPVC(t, client, pvcs[0].Name)
	assert.Equal(t, v1.ClaimBound, pvc.Status.Phase)

	// load the volume from the cloudscale.ch api and verify that it
	// has the requested size and volume type
	volume := getCloudscaleVolume(t, pvc.Spec.VolumeName)
	assert.Equal(t, 5, volume.SizeGB)
	assert.Equal(t, "ssd", volume.Type)

	// verify that our disk is luks-encrypted, formatted with ext4 and 5 GB big
	disk, err := getVolumeInfo(t, pod, pvc.Spec.VolumeName)
	assert.NoError(t, err)
	assert.Equal(t, "ext4", disk.Filesystem)
	assert.Equal(t, 5*driver.GB, disk.DeviceSize)
	assert.Equal(t, "LUKS1", disk.Luks)
	assert.Equal(t, "Filesystem", disk.PVCVolumeMode)
	assert.Equal(t, "aes-xts-plain64", disk.Cipher)
	assert.Equal(t, 512, disk.Keysize)
	assert.Equal(t, 5*driver.GB-luksOverhead, disk.FilesystemSize)

	// delete the pod and the pvcs and wait until the volume was deleted from
	// the cloudscale.ch account; this check is necessary to test that the
	// csi-plugin properly deletes the volume from cloudscale.ch
	cleanup(t, podDescriptor)
	waitCloudscaleVolumeDeleted(t, pvc.Spec.VolumeName)
}

func TestPod_Single_Bulk_Luks_Volume(t *testing.T) {
	podDescriptor := TestPodDescriptor{
		Kind: "Pod",
		Name: pseudoUuid(),
		Volumes: []TestPodVolume{
			{
				ClaimName:    "csi-pod-bulk-luks-pvc",
				SizeGB:       100,
				StorageClass: "cloudscale-volume-bulk-luks",
				LuksKey:      "secret",
			},
		},
	}

	// submit the pod and the pvc
	pod := makeKubernetesPod(t, podDescriptor)
	pvcs := makeKubernetesPVCs(t, podDescriptor)
	assert.Equal(t, 1, len(pvcs))

	// wait for the pod to be running and verify that the pvc is bound
	waitForPod(t, client, pod.Name)
	pvc := getPVC(t, client, pvcs[0].Name)
	assert.Equal(t, v1.ClaimBound, pvc.Status.Phase)

	// load the volume from the cloudscale.ch api and verify that it
	// has the requested size and volume type
	volume := getCloudscaleVolume(t, pvc.Spec.VolumeName)
	assert.Equal(t, 100, volume.SizeGB)
	assert.Equal(t, "bulk", volume.Type)

	// verify that our disk is luks-encrypted, formatted with ext4 and 5 GB big
	disk, err := getVolumeInfo(t, pod, pvc.Spec.VolumeName)
	assert.NoError(t, err)
	assert.Equal(t, "ext4", disk.Filesystem)
	assert.Equal(t, 100*driver.GB, disk.DeviceSize)
	assert.Equal(t, "LUKS1", disk.Luks)
	assert.Equal(t, "Filesystem", disk.PVCVolumeMode)
	assert.Equal(t, "aes-xts-plain64", disk.Cipher)
	assert.Equal(t, 512, disk.Keysize)
	assert.Equal(t, 100*driver.GB-luksOverhead, disk.FilesystemSize)

	// delete the pod and the pvcs and wait until the volume was deleted from
	// the cloudscale.ch account
	cleanup(t, podDescriptor)
	waitCloudscaleVolumeDeleted(t, pvc.Spec.VolumeName)
}

var resizeCases = []struct {
	storageClass      string
	block             bool
	initialSizeGB     int
	newSizeGB         int
	LuksKey           string
	newFilesystemSize int
}{
	{"cloudscale-volume-ssd", false, 5, 6, "", 6 * driver.GB},
	{"cloudscale-volume-ssd", true, 7, 8, "", -1},
	{"cloudscale-volume-bulk", false, 100, 200, "", 200 * driver.GB},
	{"cloudscale-volume-ssd-luks", false, 1, 3, "secret", 3*driver.GB - luksOverhead},
}

func TestPersistentVolume_Resize(t *testing.T) {
	for _, tt := range resizeCases {
		t.Run(fmt.Sprintf("%v %v", tt.storageClass, tt.block), func(t *testing.T) {
			podDescriptor := TestPodDescriptor{
				Kind: "Pod",
				Name: pseudoUuid(),
				Volumes: []TestPodVolume{
					{
						ClaimName:    "csi-pod-ssd-pvc",
						SizeGB:       tt.initialSizeGB,
						StorageClass: tt.storageClass,
						LuksKey:      tt.LuksKey,
						Block:        tt.block,
					},
				},
			}

			// submit the pod and the pvc
			pod := makeKubernetesPod(t, podDescriptor)
			pvcs := makeKubernetesPVCs(t, podDescriptor)
			assert.Equal(t, 1, len(pvcs))

			// wait for the pod to be running and verify that the pvc is bound
			waitForPod(t, client, pod.Name)
			pvc := getPVC(t, client, pvcs[0].Name)
			assert.Equal(t, v1.ClaimBound, pvc.Status.Phase)

			claimName := pvc.Name
			createdPVC, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(context.Background(), claimName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}

			pvName := createdPVC.Spec.VolumeName
			pv, err := client.CoreV1().PersistentVolumes().Get(context.Background(), pvName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}

			if expectedSize := resource.MustParse(fmt.Sprintf("%vGi", tt.initialSizeGB)); pv.Spec.Capacity["storage"] != expectedSize {
				t.Fatalf("initial volume size (%v) is not equal to requested volume size (%v)", pv.Spec.Capacity["storage"], expectedSize)
			}

			disk, err := getVolumeInfo(t, pod, pvc.Spec.VolumeName)
			assert.NoError(t, err)
			originalFilesystemUUID := disk.FilesystemUUID

			newSize := resource.MustParse(fmt.Sprintf("%vGi", tt.newSizeGB))

			t.Log("Updating pvc to request more size")
			createdPVC.Spec.Resources.Requests = v1.ResourceList{
				v1.ResourceStorage: newSize,
			}

			updatedPVC, err := client.CoreV1().PersistentVolumeClaims(namespace).Update(context.Background(), createdPVC, metav1.UpdateOptions{})
			if err != nil {
				t.Fatal(err)
			}

			t.Logf("Waiting for volume %q to be resized ...", pvName)
			resizedPv, err := waitForVolumeCapacityChange(client, pvName, pv.Spec.Capacity)
			if err != nil {
				t.Error(err)
			}

			if resizedPv.Spec.Capacity["storage"] != newSize {
				t.Fatalf("volume size (%v) is not equal to requested volume size (%v)", pv.Spec.Capacity["storage"], newSize)
			}

			t.Logf("Waiting for volume claim %q to be resized ...", claimName)
			resizedPVC, err := waitForVolumeClaimCapacityChange(client, claimName, updatedPVC.Status.Capacity)
			if err != nil {
				t.Error(err)
			}

			if resizedPVC.Status.Capacity["storage"] != newSize {
				t.Fatalf("claim capacity (%v) is not equal to requested capacity (%v)", resizedPVC.Status.Capacity["storage"], newSize)
			}

			// wait for the node to see a larger device
			t.Logf("Waiting device %q to be resized from node perspective ...", claimName)
			waitDeviceResized(t, pod, pvc.Spec.VolumeName, tt.newSizeGB*driver.GB)

			// wait for the node to resize the filesystem of the volume which was resized by the controller
			t.Logf("Waiting for filesystem %q to be resized ...", claimName)
			waitFilesystemResized(t, pod, pvc.Spec.VolumeName, tt.newFilesystemSize)

			// verify that our disk now has the new parameters applied
			disk, err = getVolumeInfo(t, pod, pvc.Spec.VolumeName)
			assert.NoError(t, err)
			if tt.LuksKey == "" {
				assert.Equal(t, "", disk.Luks)
			} else {
				assert.Equal(t, "LUKS1", disk.Luks)
			}
			if tt.block == true {
				assert.Equal(t, "Block", disk.PVCVolumeMode)
				assert.Equal(t, "", disk.Filesystem)
			} else {
				assert.Equal(t, "Filesystem", disk.PVCVolumeMode)
				assert.Equal(t, "ext4", disk.Filesystem)
			}
			assert.Equal(t, tt.newSizeGB*driver.GB, disk.DeviceSize)
			// assert file system uuid has not changed
			assert.Equal(t, originalFilesystemUUID, disk.FilesystemUUID)

			// delete the pod and the pvcs and wait until the volume was deleted from
			// the cloudscale.ch account; this check is necessary to test that the
			// csi-plugin properly deletes the volume from cloudscale.ch
			cleanup(t, podDescriptor)
			waitCloudscaleVolumeDeleted(t, pvc.Spec.VolumeName)
		})
	}
}

func TestVolumeStats(t *testing.T) {
	pvcName := fmt.Sprintf("csi-pvc-stats-%v", pseudoUuid())
	podName := pseudoUuid()
	podDescriptor := TestPodDescriptor{
		Kind: "Pod",
		Name: podName,
		Volumes: []TestPodVolume{
			{
				ClaimName:    pvcName,
				SizeGB:       1,
				StorageClass: "cloudscale-volume-ssd",
			},
		},
	}

	// submit the pod and the p
	pod := makeKubernetesPod(t, podDescriptor)
	pvcs := makeKubernetesPVCs(t, podDescriptor)
	assert.Equal(t, 1, len(pvcs))

	// wait for the pod to be running and verify that the p is bound
	waitForPod(t, client, pod.Name)

	// construct the metric url
	nodeName := getPod(t, client, podName).Spec.NodeName
	uri := fmt.Sprintf("%s/api/v1/nodes/%s/proxy/metrics", config.Host, nodeName)
	t.Logf("Using the following metrics url: %v", uri)

	// wait until the metrics for the volume are available
	metrics, err := waitForMetric(t, uri, "kubelet_volume_stats_capacity_bytes", pvcName)
	if err != nil {
		t.Fatal(err)
	}

	deltaCapacity := 100.0 * driver.MB
	assertMetric(t, metrics, "kubelet_volume_stats_capacity_bytes", pvcName, 1*driver.GB, deltaCapacity)
	assertMetric(t, metrics, "kubelet_volume_stats_available_bytes", pvcName, 1*driver.GB, deltaCapacity)
	assertMetric(t, metrics, "kubelet_volume_stats_used_bytes", pvcName, 20*driver.MB, deltaCapacity)
	deltaInode := 100.0
	assertMetric(t, metrics, "kubelet_volume_stats_inodes", pvcName, 65536, deltaInode)
	assertMetric(t, metrics, "kubelet_volume_stats_inodes_free", pvcName, 65525, deltaInode)
	assertMetric(t, metrics, "kubelet_volume_stats_inodes_used", pvcName, 11, deltaInode)
}

func setup() error {
	// if you want to change the loading rules (which files in which order),
	// you can do so here
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()

	// if you want to change override values or bind them to flags, there are
	// methods to help you
	configOverrides := &clientcmd.ConfigOverrides{}

	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	var err error
	config, err = kubeConfig.ClientConfig()
	if err != nil {
		return err
	}

	// create the clientset
	client, err = kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	// create test namespace
	_, err = client.CoreV1().Namespaces().Create(
		context.Background(),
		&v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			}},
		metav1.CreateOptions{},
	)

	if err != nil {
		return err
	}

	// create cloudscale client with the secret deployed into the kube-system namespace
	secret, err := client.CoreV1().Secrets("kube-system").Get(context.Background(), "cloudscale", metav1.GetOptions{})
	if err != nil {
		return err
	}
	cloudscaleToken := string(secret.Data["access-token"])
	tokenSource := oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: cloudscaleToken,
	})
	oauthClient := oauth2.NewClient(context.Background(), tokenSource)

	cloudscaleClient = cloudscale.NewClient(oauthClient)

	return nil
}

func teardown() error {
	// delete all test resources
	err := client.CoreV1().Namespaces().Delete(context.Background(), namespace, metav1.DeleteOptions{})
	if err != nil && !(kubeerrors.IsNotFound(err) || kubeerrors.IsAlreadyExists(err)) {
		return err
	}

	return nil
}

func strPtr(s string) *string {
	return &s
}

// deletes resources (pods, deployment, pvcs) for the given TestPodDescriptor from kubernetes
// NOTE: does not wait for the resources to be deleted
func cleanup(t *testing.T, pod TestPodDescriptor) {
	if pod.Kind == "Deployment" {
		err := client.AppsV1().Deployments(namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
		assert.NoError(t, err)
	} else {
		err := client.CoreV1().Pods(namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
		assert.NoError(t, err)
	}
	for _, volume := range pod.Volumes {
		err := client.CoreV1().PersistentVolumeClaims(namespace).Delete(context.Background(), volume.ClaimName, metav1.DeleteOptions{})
		assert.NoError(t, err)
	}
}

// creates a kubernetes pod from the given TestPodDescriptor
func makeKubernetesPod(t *testing.T, pod TestPodDescriptor) *v1.Pod {

	volumeMounts := make([]v1.VolumeMount, 0)
	volumeDevices := make([]v1.VolumeDevice, 0)
	volumes := make([]v1.Volume, 0)
	luksSecrets := make([]v1.Secret, 0)

	for i, volume := range pod.Volumes {
		volumeName := fmt.Sprintf("volume-%v", i)
		if !volume.Block {
			volumeMounts = append(volumeMounts, v1.VolumeMount{
				MountPath: fmt.Sprintf("/data-%v", i),
				Name:      volumeName,
			})
		} else {
			volumeDevices = append(volumeDevices, v1.VolumeDevice{
				DevicePath: fmt.Sprintf("/dev/xvd-%v", i),
				Name:       volumeName,
			})
		}
		volumes = append(volumes, v1.Volume{
			Name: volumeName,
			VolumeSource: v1.VolumeSource{
				PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
					ClaimName: volume.ClaimName,
				},
			},
		})
		if volume.LuksKey != "" {
			luksSecrets = append(luksSecrets, v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%v-luks-key", volume.ClaimName),
					Namespace: namespace,
				},
				Type: v1.SecretTypeOpaque,
				StringData: map[string]string{
					"luksKey": volume.LuksKey,
				},
			})
		}
	}

	for _, secret := range luksSecrets {
		t.Logf("Creating luks-secret %v", secret.Name)
		_, err := client.CoreV1().Secrets(namespace).Create(context.Background(), &secret, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}

	kubernetesPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: namespace,
		},
		Spec: v1.PodSpec{
			// use google's pause container instead of a sleeping busybox
			// reasoning: the pause container properly terminates when the container runtime
			// signals TERM; a sleeping busybox will not and it will take a while before the
			// container is killed, unless we were to explicitly handle the TERM signal
			Containers: []v1.Container{
				{
					Name:          "pause",
					Image:         "gcr.io/google-containers/pause-amd64:3.1",
					VolumeMounts:  volumeMounts,
					VolumeDevices: volumeDevices,
				},
			},
			Volumes: volumes,
		},
	}

	t.Log("Creating pod")
	_, err := client.CoreV1().Pods(namespace).Create(context.Background(), kubernetesPod, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	return kubernetesPod
}

// creates a kubernetes deployment from the given TestPodDescriptor
func makeKubernetesDeployment(t *testing.T, pod TestPodDescriptor) *appsv1.Deployment {
	replicaCount := new(int32)
	*replicaCount = 1

	volumeMounts := make([]v1.VolumeMount, 0)
	volumes := make([]v1.Volume, 0)

	for i, volume := range pod.Volumes {
		volumeName := fmt.Sprintf("volume-%v", i)
		volumeMounts = append(volumeMounts, v1.VolumeMount{
			MountPath: fmt.Sprintf("/data-%v", i),
			Name:      volumeName,
		})
		volumes = append(volumes, v1.Volume{
			Name: volumeName,
			VolumeSource: v1.VolumeSource{
				PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
					ClaimName: volume.ClaimName,
				},
			},
		})
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: pod.Name,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: replicaCount,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": pod.Name,
				},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": pod.Name,
					},
				},
				Spec: v1.PodSpec{
					// use google's pause container instead of a sleeping busybox
					// reasoning: the pause container properly terminates when the container runtime
					// signals TERM; a sleeping busybox will not and it will take a while before the
					// container is killed, unless we were to explicitly handle the TERM signal
					Containers: []v1.Container{
						{
							Name:         "pause",
							Image:        "gcr.io/google-containers/pause-amd64:3.1",
							VolumeMounts: volumeMounts,
						},
					},
					Volumes: volumes,
				},
			},
		},
	}

	t.Logf("Creating deployment %v", pod.Name)
	_, err := client.AppsV1().Deployments(namespace).Create(context.Background(), deployment, metav1.CreateOptions{})
	assert.NoError(t, err)

	return deployment
}

// creates kubernetes pvcs from the given TestPodDescriptor
func makeKubernetesPVCs(t *testing.T, pod TestPodDescriptor) []*v1.PersistentVolumeClaim {
	pvcs := make([]*v1.PersistentVolumeClaim, 0)

	for _, volume := range pod.Volumes {
		volMode := v1.PersistentVolumeFilesystem
		if volume.Block {
			volMode = v1.PersistentVolumeBlock
		}

		pvcs = append(pvcs, &v1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name: volume.ClaimName,
			},
			Spec: v1.PersistentVolumeClaimSpec{
				VolumeMode: &volMode,
				AccessModes: []v1.PersistentVolumeAccessMode{
					v1.ReadWriteOnce,
				},
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceStorage: resource.MustParse(fmt.Sprintf("%vGi", volume.SizeGB)),
					},
				},
				StorageClassName: strPtr(volume.StorageClass),
			},
		})
	}

	t.Log("Creating pvc")
	for _, pvc := range pvcs {
		_, err := client.CoreV1().PersistentVolumeClaims(namespace).Create(context.Background(), pvc, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}

	return pvcs
}

// taken from https://stackoverflow.com/a/25736155
// adapted to make uuids lowercase
func pseudoUuid() (uuid string) {

	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		fmt.Println("Error: ", err)
		return
	}

	uuid = fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])

	return
}

// waitForPod waits for the given pod name to be running
func waitForPod(t *testing.T, client kubernetes.Interface, name string) {
	var err error
	stopCh := make(chan struct{})

	t.Logf("Waiting for pod %q to be running ...\n", name)

	go func() {
		select {
		case <-time.After(time.Minute * 5):
			err = errors.New("timing out waiting for pod state")
			close(stopCh)
		case <-stopCh:
		}
	}()

	watchlist := cache.NewListWatchFromClient(client.CoreV1().RESTClient(),
		"pods", namespace, fields.Everything())
	_, controller := cache.NewInformer(watchlist, &v1.Pod{}, time.Second*1,
		cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(o, n interface{}) {
				pod := n.(*v1.Pod)
				if name != pod.Name {
					return
				}

				if pod.Status.Phase == v1.PodFailed || pod.Status.Phase == v1.PodSucceeded {
					err = errors.New("pod status is Failed or in Succeeded status (terminated)")
					close(stopCh)
					return
				}

				if pod.Status.Phase == v1.PodRunning {
					close(stopCh)
					return
				}
			},
		})

	controller.Run(stopCh)
	assert.NoError(t, err)
}

// waitForVolumeCapacityChange waits for the given volume's capacity to be changed
func waitForVolumeCapacityChange(client kubernetes.Interface, name string, resourceList v1.ResourceList) (*v1.PersistentVolume, error) {
	var err error
	var pv *v1.PersistentVolume
	stopCh := make(chan struct{})

	go func() {
		select {
		case <-time.After(time.Minute * 2):
			err = errors.New("timing out waiting for pv capcity change")
			close(stopCh)
		case <-stopCh:
		}
	}()

	watchlist := cache.NewListWatchFromClient(client.CoreV1().RESTClient(),
		"persistentvolumes", v1.NamespaceAll, fields.Everything())
	_, controller := cache.NewInformer(watchlist, &v1.PersistentVolume{}, time.Second*1,
		cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(o, n interface{}) {
				volume := n.(*v1.PersistentVolume)
				if name != volume.Name {
					return
				}
				if volume.Status.Phase == v1.VolumeFailed {
					err = errors.New("Persistent volume status is Failed")
					close(stopCh)
					return
				}

				if volume.Status.Phase == v1.VolumeBound && volume.Spec.Capacity["storage"] != resourceList["storage"] {
					pv = volume
					close(stopCh)
					return
				}
			},
		})

	controller.Run(stopCh)
	return pv, err
}

// waitForVolumeClaimCapacityChange waits for the given volume claim's capacity to be changed
func waitForVolumeClaimCapacityChange(client kubernetes.Interface, name string, resourceList v1.ResourceList) (*v1.PersistentVolumeClaim, error) {
	var err error
	var pvc *v1.PersistentVolumeClaim
	stopCh := make(chan struct{})

	go func() {
		select {
		case <-time.After(time.Minute * 2):
			err = errors.New("timing out waiting for pvc capcity change")
			close(stopCh)
		case <-stopCh:
		}
	}()

	watchlist := cache.NewListWatchFromClient(client.CoreV1().RESTClient(),
		"persistentvolumeclaims", namespace, fields.Everything())
	_, controller := cache.NewInformer(watchlist, &v1.PersistentVolumeClaim{}, time.Second*1,
		cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(o, n interface{}) {
				claim := n.(*v1.PersistentVolumeClaim)
				if name != claim.Name {
					return
				}
				if claim.Status.Phase == v1.ClaimLost {
					err = errors.New("Persistent volume claim status is Lost")
					close(stopCh)
					return
				}
				if claim.Status.Phase == v1.ClaimBound && claim.Status.Capacity["storage"] != resourceList["storage"] {
					pvc = claim
					close(stopCh)
					return
				}
			},
		})

	controller.Run(stopCh)
	return pvc, err
}

// waitForMetric waits for the the given metric to be present at the location specified by uri
func waitForMetric(t *testing.T, uri string, metricName string, pvcName string) (metrics *MetricsSet, err error) {
	start := time.Now()

	for {
		result := client.CoreV1().RESTClient().
			Get().
			RequestURI(uri).
			Do(context.Background())

		if err := result.Error(); err != nil {
			return nil, err
		}

		metrics := generateMetricsObject(result)
		_, err := metrics.findByLabel(metricName, pvcName)

		if err != nil {
			if time.Now().UnixNano()-start.UnixNano() > (5 * time.Minute).Nanoseconds() {
				err = errors.New(fmt.Sprintf("timeout exceeded while waiting for metric %v for pvc %v", metricName, pvcName))
				return nil, err
			} else {
				t.Logf("Waiting for metric, currently: %v", err)
				time.Sleep(15 * time.Second)
			}
		} else {
			return &metrics, nil
		}
	}
}

// appSelector returns a selector that selects deployed applications with the
// given name
func appSelector(appName string) (labels.Selector, error) {
	selector := labels.NewSelector()
	appRequirement, err := labels.NewRequirement("app", selection.Equals, []string{appName})
	if err != nil {
		return nil, err
	}

	selector = selector.Add(
		*appRequirement,
	)

	return selector, nil
}

// loads the pvc with the given name from kubernetes
func getPVC(t *testing.T, client kubernetes.Interface, name string) *v1.PersistentVolumeClaim {
	claim, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(context.Background(), name, metav1.GetOptions{})
	assert.NoError(t, err)
	return claim
}

// loads the pod with the given name from kubernetes
func getPod(t *testing.T, client kubernetes.Interface, name string) *v1.Pod {
	pod, err := client.CoreV1().Pods(namespace).Get(context.Background(), name, metav1.GetOptions{})
	assert.NoError(t, err)
	return pod
}

// loads the volume with the given name from the cloudscale.ch API
func getCloudscaleVolume(t *testing.T, volumeName string) cloudscale.Volume {
	ctx, _ := context.WithTimeout(context.Background(), 30*time.Second)
	volumes, err := cloudscaleClient.Volumes.List(ctx, cloudscale.WithNameFilter(volumeName))

	assert.NoError(t, err)
	assert.Equal(t, 1, len(volumes))
	return volumes[0]
}

// waits until the volume with the given name was deleted from the cloudscale.ch account
func waitCloudscaleVolumeDeleted(t *testing.T, volumeName string) {
	start := time.Now()

	for {
		ctx, _ := context.WithTimeout(context.Background(), 30*time.Second)
		volumes, err := cloudscaleClient.Volumes.List(ctx, cloudscale.WithNameFilter(volumeName))
		if len(volumes) == 0 {
			t.Logf("volume %v is deleted on cloudscale", volumeName)
			return
		}
		if err != nil {
			if cloudscaleErr, ok := err.(*cloudscale.ErrorResponse); ok {
				if cloudscaleErr.StatusCode == http.StatusNotFound {
					t.Logf("volume %v is deleted on cloudscale", volumeName)
					return
				}
			}
		}
		if time.Now().UnixNano()-start.UnixNano() > (5 * time.Minute).Nanoseconds() {
			t.Errorf("timeout exceeded while waiting for volume %v to be deleted from cloudscale", volumeName)
			return
		} else {
			t.Logf("volume %v not deleted on cloudscale yet; awaiting deletion", volumeName)
			time.Sleep(5 * time.Second)
		}
	}
}

// waits until the device was resized on the node after the volume itself was resized by the controller
func waitDeviceResized(t *testing.T, pod *v1.Pod, volumeName string, expectedDeviceSize int) {
	start := time.Now()

	for {
		disk, err := getVolumeInfo(t, pod, volumeName)
		assert.NoError(t, err)

		if disk.DeviceSize == expectedDeviceSize {
			t.Logf("device %v was resized", volumeName)
			return
		}

		if time.Now().UnixNano()-start.UnixNano() > (5 * time.Minute).Nanoseconds() {
			t.Errorf("timeout exceeded while waiting device %v to be resized from cloudscale", volumeName)
			return
		} else {
			t.Logf("device %v was not resized yet; awaiting resize operation on the node\nexpectedDeviceSize = %v", volumeName, expectedDeviceSize)
			time.Sleep(5 * time.Second)
		}
	}
}

// waits until the volume's filesystem was resized on the node after the volume itself was resized by the controller
func waitFilesystemResized(t *testing.T, pod *v1.Pod, volumeName string, expectedFilesystemSize int) {
	start := time.Now()

	for {
		disk, err := getVolumeInfo(t, pod, volumeName)
		assert.NoError(t, err)

		if disk.FilesystemSize == expectedFilesystemSize {
			t.Logf("filesystem on volume %v was resized", volumeName)
			return
		}

		if time.Now().UnixNano()-start.UnixNano() > (5 * time.Minute).Nanoseconds() {
			t.Errorf("timeout exceeded while waiting for filesystem on volume %v to be resized from cloudscale", volumeName)
			return
		} else {
			t.Logf("filesystem on volume %v was not resized yet; awaiting resize operation on the node\nexpectedFilesystemSize = %v", volumeName, expectedFilesystemSize)
			time.Sleep(5 * time.Second)
		}
	}
}

// returns the name of the node where the given pod is running on
func getNodeName(podNamespace string, podName string) (string, error) {
	pod, err := client.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return pod.Spec.NodeName, nil
}

// returns the diskinfo for the volume with the given name mounted into the given pod
func getVolumeInfo(t *testing.T, pod *v1.Pod, volumeName string) (DiskInfo, error) {
	node, err := getNodeName(pod.Namespace, pod.Name)
	if err != nil {
		return DiskInfo{}, err
	}
	diskInfo, err := getVolumeInfoFromNode(t, node)
	if err != nil {
		return DiskInfo{}, err
	}
	for _, disk := range diskInfo {
		if disk.PVCName == volumeName {
			return disk, nil
		}
	}
	return DiskInfo{}, fmt.Errorf("cannot find volume with name %v on node %v", volumeName, node)
}

// inspects the node and returns information about the disks from the node's perspective
func getVolumeInfoFromNode(t *testing.T, nodeName string) ([]DiskInfo, error) {
	diskInfo := make([]DiskInfo, 0)

	pods, err := client.CoreV1().Pods("kube-system").List(context.Background(), metav1.ListOptions{
		LabelSelector: "app=csi-cloudscale-node, role=csi-cloudscale",
	})
	if err != nil {
		t.Errorf("unable to list pods in namespace kube-system: %v", err)
		return diskInfo, err
	}
	var csiPluginPod *v1.Pod
	for _, pod := range pods.Items {
		tmpPod := pod
		if tmpPod.Spec.NodeName == nodeName {
			csiPluginPod = &tmpPod
			break
		}
	}
	if csiPluginPod == nil {
		t.Errorf("unable to find csi-cloudscale-node pod on node %v", nodeName)
		return diskInfo, errors.New("unable to find csi-cloudscale-node pod on node " + nodeName)
	}

	output, err := ExecCommand(csiPluginPod.Namespace, csiPluginPod.Name, "/bin/csi-diskinfo.sh")
	log.Print("output:")
	log.Print(output)
	if err != nil {
		return diskInfo, err
	}
	err = json.Unmarshal([]byte(output), &diskInfo)
	return diskInfo, err
}

// taken from https://github.com/zalando-incubator/postgres-operator/blob/master/pkg/cluster/exec.go
// and adapted to work for this scenario
// ExecCommand executes arbitrary command inside the pod
func ExecCommand(podNamespace string, podName string, command ...string) (string, error) {
	log.Printf("executing command %q", strings.Join(command, " "))

	var (
		execOut bytes.Buffer
		execErr bytes.Buffer
	)

	pod, err := client.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("could not get pod info: %v", err)
	}

	// iterate through all containers looking for the one running the csi plugin
	targetContainer := -1
	for i, cr := range pod.Spec.Containers {
		if cr.Name == "csi-cloudscale-plugin" {
			targetContainer = i
			break
		}
	}

	if targetContainer < 0 {
		return "", fmt.Errorf("could not find %s container to exec to", "csi-cloudscale-plugin")
	}

	req := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(podNamespace).
		SubResource("exec").
		Param("container", pod.Spec.Containers[targetContainer].Name).
		Param("command", strings.Join(command, " ")).
		Param("stdin", "false").
		Param("stdout", "true").
		Param("stderr", "true").
		Param("tty", "false")

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("failed to init executor: %v", err)
	}

	err = exec.Stream(remotecommand.StreamOptions{
		Stdout: &execOut,
		Stderr: &execErr,
		Tty:    false,
	})

	if err != nil {
		return "", fmt.Errorf("could not execute: %v", err)
	}

	if execErr.Len() > 0 {
		return "", fmt.Errorf("stderr: %v", execErr.String())
	}

	return execOut.String(), nil
}

// Metrics Handling

type MetricsSet struct {
	entries []MetricEntry
}

type MetricEntry struct {
	metricName string
	labels     string
	value      string
}

func assertMetric(t *testing.T, metrics *MetricsSet, name string, substring string, expected float64, delta float64) {
	metric, err := metrics.findByLabel(name, substring)
	if err != nil {
		t.Errorf("Metric not found %v", name)
	}
	float, err := strconv.ParseFloat(metric.value, 64)
	if err != nil {
		t.Error(err)
	}
	assert.InDelta(t, expected, float, delta)
}

func (km *MetricsSet) filterByName(name string) (ret []MetricEntry) {
	for _, s := range km.entries {
		if s.metricName == name {
			ret = append(ret, s)
		}
	}
	return
}

func (km *MetricsSet) findByLabel(name string, dictSubstring string) (*MetricEntry, error) {
	for _, s := range km.filterByName(name) {
		if strings.Contains(s.labels, dictSubstring) {
			return &s, nil
		}
	}
	return nil, errors.New(fmt.Sprintf("Could not find metric with name %v and label containg %v", name, dictSubstring))
}

func generateMetricsObject(result rest.Result) MetricsSet {
	entries := make([]MetricEntry, 1000)
	rawBody, _ := result.Raw()
	scanner := bufio.NewScanner(strings.NewReader(string(rawBody)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		metric := generateMetricEntry(line)
		entries = append(entries, metric)
	}

	return MetricsSet{entries}
}

func generateMetricEntry(line string) MetricEntry {
	split := strings.Split(line, " ")
	if strings.Contains(split[0], "{") {
		start := strings.Index(split[0], "{")
		end := strings.Index(split[0], "}")
		metricLabels := split[0][start : end+1]
		name := split[0][:start]
		return MetricEntry{name, metricLabels, split[1]}
	}
	return MetricEntry{split[0], "", split[1]}
}
