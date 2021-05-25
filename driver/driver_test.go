/*
Copyright cloudscale.ch
Copyright 2018 DigitalOcean

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

package driver

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/cloudscale-ch/cloudscale-go-sdk"
	"github.com/kubernetes-csi/csi-test/pkg/sanity"
	"github.com/sirupsen/logrus"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

var DefaultZone = cloudscale.Zone{Slug: "dev1"}

func TestDriverSuite(t *testing.T) {
	socket := "/tmp/csi.sock"
	endpoint := "unix://" + socket
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		t.Fatalf("failed to remove unix domain socket file %s, error: %s", socket, err)
	}

	serverId := "987654"
	initialServers := map[string]*cloudscale.Server{
		serverId: {UUID: serverId},
	}
	cloudscaleClient := NewFakeClient(initialServers)
	driver := &Driver{
		endpoint:         endpoint,
		serverId:         serverId,
		region:           DefaultZone.Slug,
		cloudscaleClient: cloudscaleClient,
		mounter: &fakeMounter{
			mounted: map[string]string{},
		},
		log: logrus.New().WithField("test_enabed", true),
	}
	defer driver.Stop()

	go driver.Run()

	targetDir := os.TempDir() + "/csi-target"
	stagingDir := os.TempDir() + "/csi-staging"

	cfg := &sanity.Config{
		Address:        endpoint,
		TestVolumeSize: 50 * 1024 * 1024 * 1024,
		TargetPath:     targetDir,
		StagingPath:    stagingDir,
	}
	cfg.TestNodeVolumeAttachLimit = true

	sanity.Test(t, cfg)
}

func NewFakeClient(initialServers map[string]*cloudscale.Server) *cloudscale.Client {
	userAgent := "cloudscale/" + "fake"
	fakeClient := &cloudscale.Client{BaseURL: nil, UserAgent: userAgent}

	fakeClient.Servers = FakeServerServiceOperations{
		fakeClient: fakeClient,
		servers:    initialServers,
	}
	fakeClient.Volumes = FakeVolumeServiceOperations{
		fakeClient: fakeClient,
		volumes:    make(map[string]*cloudscale.Volume),
	}

	return fakeClient
}

type fakeMounter struct {
	mounted map[string]string
}

func (f *fakeMounter) Format(source string, fsType string, luksContext LuksContext) error {
	return nil
}

func (f *fakeMounter) Mount(source string, target string, fsType string, luksContext LuksContext, options ...string) error {
	f.mounted[target] = source
	return nil
}

func (f *fakeMounter) Unmount(target string, luksContext LuksContext) error {
	delete(f.mounted, target)
	return nil
}

func (f *fakeMounter) IsFormatted(source string, luksContext LuksContext) (bool, error) {
	return true, nil
}
func (f *fakeMounter) IsMounted(target string) (bool, error) {
	_, ok := f.mounted[target]
	return ok, nil
}

func (f *fakeMounter) GetStatistics(volumePath string) (volumeStatistics, error) {
	return volumeStatistics{
		availableBytes: 3 * 1024 * 1024 * 1024,
		totalBytes:     10 * 1024 * 1024 * 1024,
		usedBytes:      7 * 1024 * 1024 * 1024,

		availableInodes: 3000,
		totalInodes:     10000,
		usedInodes:      7000,
	}, nil
}

func (f *fakeMounter) FinalizeVolumeAttachmentAndFindPath(logger *logrus.Entry, target string) (*string, error) {
	path := "SomePath"
	return &path, nil
}

type FakeVolumeServiceOperations struct {
	fakeClient *cloudscale.Client
	volumes    map[string]*cloudscale.Volume
}

func (f FakeVolumeServiceOperations) Create(ctx context.Context, createRequest *cloudscale.VolumeRequest) (*cloudscale.Volume, error) {
	id := randString(32)
	vol := &cloudscale.Volume{
		UUID:        id,
		Name:        createRequest.Name,
		SizeGB:      createRequest.SizeGB,
		Type:        createRequest.Type,
		ServerUUIDs: createRequest.ServerUUIDs,
	}
	vol.Zone = DefaultZone
	if vol.ServerUUIDs == nil {
		noservers := make([]string, 0, 1)
		vol.ServerUUIDs = &noservers
	}

	f.volumes[id] = vol

	return vol, nil
}

func (f FakeVolumeServiceOperations) Get(ctx context.Context, volumeID string) (*cloudscale.Volume, error) {
	vol, ok := f.volumes[volumeID]
	if ok != true {
		return nil, generateNotFoundError()
	}
	return vol, nil
}

func (f FakeVolumeServiceOperations) List(ctx context.Context, modifiers ...cloudscale.ListRequestModifier) ([]cloudscale.Volume, error) {
	var volumes []cloudscale.Volume

	for _, vol := range f.volumes {
		volumes = append(volumes, *vol)
	}

	if len(modifiers) == 0 {
		return volumes, nil
	}
	if len(modifiers) > 1 {
		panic("implement me (support for more than one modifier)")
	}

	params := extractParams(modifiers)

	if filterName := params.Get("name"); filterName != "" {
		filtered := make([]cloudscale.Volume, 0, 1)
		for _, vol := range volumes {
			if vol.Name == filterName {
				filtered = append(filtered, vol)
			}
		}
		return filtered, nil
	}

	panic("implement me (support for unknown param)")
}

func extractParams(modifiers []cloudscale.ListRequestModifier) url.Values {
	// undoing the cloudscale.WithNameFilter(volumeName) magic

	modifierFunc := modifiers[0]
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	modifierFunc(req)
	params, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		panic("unexpected error")
	}
	return params
}

func (f FakeVolumeServiceOperations) Update(ctx context.Context, volumeID string, updateRequest *cloudscale.VolumeRequest) error {
	vol, ok := f.volumes[volumeID]
	if ok != true {
		return generateNotFoundError()
	}

	if updateRequest.ServerUUIDs != nil {
		if serverUUIDs := *updateRequest.ServerUUIDs; serverUUIDs != nil {
			if len(serverUUIDs) > 1 {
				return errors.New("multi attach is not implemented")
			}
			if len(serverUUIDs) == 1 {
				for _, serverUUID := range serverUUIDs {
					_, err := f.fakeClient.Servers.Get(nil, serverUUID)
					if err != nil {
						return err
					}

					volumesCount := getVolumesPerServer(f, serverUUID)
					if volumesCount >= defaultMaxVolumesPerNode {
						return &cloudscale.ErrorResponse{
							StatusCode: 400,
							Message:    map[string]string{"detail": "Due to internal limitations, it is currently not possible to attach more than 128 volumes"},
						}
					}
				}
			}

			vol.ServerUUIDs = &serverUUIDs
			return nil
		}
	}
	if vol.SizeGB < updateRequest.SizeGB {
		vol.SizeGB = updateRequest.SizeGB
		return nil
	}
	panic("implement me")
}

func getVolumesPerServer(f FakeVolumeServiceOperations, serverUUID string) int {
	volumesCount := 0
	for _, v := range f.volumes {
		for _, uuid := range *v.ServerUUIDs {
			if uuid == serverUUID {
				volumesCount++
			}
		}
	}
	return volumesCount
}

func (f FakeVolumeServiceOperations) Delete(ctx context.Context, volumeID string) error {
	delete(f.volumes, volumeID)
	return nil
}

type FakeServerServiceOperations struct {
	fakeClient *cloudscale.Client
	servers    map[string]*cloudscale.Server
}

func (f FakeServerServiceOperations) Create(ctx context.Context, createRequest *cloudscale.ServerRequest) (*cloudscale.Server, error) {
	panic("implement me")
}

func (f FakeServerServiceOperations) Get(ctx context.Context, serverID string) (*cloudscale.Server, error) {
	server, ok := f.servers[serverID]
	if ok != true {
		return nil, generateNotFoundError()
	}
	return server, nil
}

func (f FakeServerServiceOperations) Update(ctx context.Context, serverID string, updateRequest *cloudscale.ServerUpdateRequest) error {
	panic("implement me")
}

func (f FakeServerServiceOperations) Delete(ctx context.Context, serverID string) error {
	panic("implement me")
}

func (f FakeServerServiceOperations) List(ctx context.Context, modifiers ...cloudscale.ListRequestModifier) ([]cloudscale.Server, error) {
	panic("implement me")
}

func (f FakeServerServiceOperations) Reboot(ctx context.Context, serverID string) error {
	panic("implement me")
}

func (f FakeServerServiceOperations) Start(ctx context.Context, serverID string) error {
	panic("implement me")
}

func (f FakeServerServiceOperations) Stop(ctx context.Context, serverID string) error {
	panic("implement me")
}

func generateNotFoundError() *cloudscale.ErrorResponse {
	return &cloudscale.ErrorResponse{
		StatusCode: 404,
		Message:    map[string]string{"detail": "not found"},
	}
}

func randString(n int) string {
	const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Intn(len(letterBytes))]
	}
	return string(b)
}
