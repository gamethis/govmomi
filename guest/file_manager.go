/*
Copyright (c) 2015-2017 VMware, Inc. All Rights Reserved.

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

package guest

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"sync"

	"github.com/vmware/govmomi/internal"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

type FileManager struct {
	types.ManagedObjectReference

	vm types.ManagedObjectReference

	c *vim25.Client

	mu    *sync.Mutex
	hosts map[string]string
}

func (m FileManager) Reference() types.ManagedObjectReference {
	return m.ManagedObjectReference
}

func (m FileManager) ChangeFileAttributes(ctx context.Context, auth types.BaseGuestAuthentication, guestFilePath string, fileAttributes types.BaseGuestFileAttributes) error {
	req := types.ChangeFileAttributesInGuest{
		This:           m.Reference(),
		Vm:             m.vm,
		Auth:           auth,
		GuestFilePath:  guestFilePath,
		FileAttributes: fileAttributes,
	}

	_, err := methods.ChangeFileAttributesInGuest(ctx, m.c, &req)
	return err
}

func (m FileManager) CreateTemporaryDirectory(ctx context.Context, auth types.BaseGuestAuthentication, prefix, suffix string, path string) (string, error) {
	req := types.CreateTemporaryDirectoryInGuest{
		This:          m.Reference(),
		Vm:            m.vm,
		Auth:          auth,
		Prefix:        prefix,
		Suffix:        suffix,
		DirectoryPath: path,
	}

	res, err := methods.CreateTemporaryDirectoryInGuest(ctx, m.c, &req)
	if err != nil {
		return "", err
	}

	return res.Returnval, nil
}

func (m FileManager) CreateTemporaryFile(ctx context.Context, auth types.BaseGuestAuthentication, prefix, suffix string, path string) (string, error) {
	req := types.CreateTemporaryFileInGuest{
		This:          m.Reference(),
		Vm:            m.vm,
		Auth:          auth,
		Prefix:        prefix,
		Suffix:        suffix,
		DirectoryPath: path,
	}

	res, err := methods.CreateTemporaryFileInGuest(ctx, m.c, &req)
	if err != nil {
		return "", err
	}

	return res.Returnval, nil
}

func (m FileManager) DeleteDirectory(ctx context.Context, auth types.BaseGuestAuthentication, directoryPath string, recursive bool) error {
	req := types.DeleteDirectoryInGuest{
		This:          m.Reference(),
		Vm:            m.vm,
		Auth:          auth,
		DirectoryPath: directoryPath,
		Recursive:     recursive,
	}

	_, err := methods.DeleteDirectoryInGuest(ctx, m.c, &req)
	return err
}

func (m FileManager) DeleteFile(ctx context.Context, auth types.BaseGuestAuthentication, filePath string) error {
	req := types.DeleteFileInGuest{
		This:     m.Reference(),
		Vm:       m.vm,
		Auth:     auth,
		FilePath: filePath,
	}

	_, err := methods.DeleteFileInGuest(ctx, m.c, &req)
	return err
}

// escape hatch to disable the preference to use ESX host management IP for guest file transfer
var useGuestTransferIP = os.Getenv("GOVMOMI_USE_GUEST_TRANSFER_IP") != "false"

// TransferURL rewrites the url with a valid hostname and adds the host's thumbprint.
// The InitiateFileTransfer{From,To}Guest methods return a URL with the host set to "*" when connected directly to ESX,
// but return the address of VM's runtime host when connected to vCenter.
func (m FileManager) TransferURL(ctx context.Context, u string) (*url.URL, error) {
	turl, err := url.Parse(u)
	if err != nil {
		return nil, err
	}

	if turl.Hostname() == "*" {
		turl.Host = m.c.URL().Host // Also use Client's port, to support port forwarding
	}

	if !m.c.IsVC() {
		return turl, nil // we already connected to the ESX host and have its thumbprint
	}

	name := turl.Hostname()
	port := turl.Port()

	m.mu.Lock()
	mname, ok := m.hosts[name]
	m.mu.Unlock()

	if ok {
		turl.Host = net.JoinHostPort(mname, port)
		return turl, nil
	}

	c := property.DefaultCollector(m.c)

	var vm mo.VirtualMachine
	err = c.RetrieveOne(ctx, m.vm, []string{"name", "runtime.host"}, &vm)
	if err != nil {
		return nil, err
	}

	if vm.Runtime.Host == nil {
		return turl, nil // won't matter if the VM was powered off since the call to InitiateFileTransfer will fail
	}

	props := []string{
		"name",
		"runtime.connectionState",
		"summary.config.sslThumbprint",
		"config.virtualNicManagerInfo.netConfig",
	}

	var host mo.HostSystem
	err = c.RetrieveOne(ctx, *vm.Runtime.Host, props, &host)
	if err != nil {
		return nil, err
	}

	if host.Config == nil {
		return nil, fmt.Errorf("guest TransferURL failed for vm %q (%s): host %q (%s) config==nil, connectionState==%s",
			vm.Name, vm.Self,
			host.Name, host.Self, host.Runtime.ConnectionState)
	}

	// InitiateFileTransfer{To,From}Guest methods return an ESX host's inventory name (HostSystem.Name).
	// This name was used to add the host to vCenter and cannot be changed (unless the host is removed from inventory and added back with another name).
	// The name used when adding to VC may not resolvable by this client's DNS, so we prefer an ESX management IP.
	// However, if there is more than one management vNIC, we don't know which IP(s) the client has a route to.
	// Leave the hostname as-is in that case or if the env var has disabled the preference.
	ips := internal.HostSystemManagementIPs(host.Config.VirtualNicManagerInfo.NetConfig)
	if len(ips) == 1 && useGuestTransferIP {
		mname = ips[0].String()
		m.mu.Lock()
		m.hosts[name] = mname
		m.mu.Unlock()

		name = mname
	}

	turl.Host = net.JoinHostPort(name, port)

	m.c.SetThumbprint(turl.Host, host.Summary.Config.SslThumbprint)

	return turl, nil
}

func (m FileManager) InitiateFileTransferFromGuest(ctx context.Context, auth types.BaseGuestAuthentication, guestFilePath string) (*types.FileTransferInformation, error) {
	req := types.InitiateFileTransferFromGuest{
		This:          m.Reference(),
		Vm:            m.vm,
		Auth:          auth,
		GuestFilePath: guestFilePath,
	}

	res, err := methods.InitiateFileTransferFromGuest(ctx, m.c, &req)
	if err != nil {
		return nil, err
	}

	return &res.Returnval, nil
}

func (m FileManager) InitiateFileTransferToGuest(ctx context.Context, auth types.BaseGuestAuthentication, guestFilePath string, fileAttributes types.BaseGuestFileAttributes, fileSize int64, overwrite bool) (string, error) {
	req := types.InitiateFileTransferToGuest{
		This:           m.Reference(),
		Vm:             m.vm,
		Auth:           auth,
		GuestFilePath:  guestFilePath,
		FileAttributes: fileAttributes,
		FileSize:       fileSize,
		Overwrite:      overwrite,
	}

	res, err := methods.InitiateFileTransferToGuest(ctx, m.c, &req)
	if err != nil {
		return "", err
	}

	return res.Returnval, nil
}

func (m FileManager) ListFiles(ctx context.Context, auth types.BaseGuestAuthentication, filePath string, index int32, maxResults int32, matchPattern string) (*types.GuestListFileInfo, error) {
	req := types.ListFilesInGuest{
		This:         m.Reference(),
		Vm:           m.vm,
		Auth:         auth,
		FilePath:     filePath,
		Index:        index,
		MaxResults:   maxResults,
		MatchPattern: matchPattern,
	}

	res, err := methods.ListFilesInGuest(ctx, m.c, &req)
	if err != nil {
		return nil, err
	}

	return &res.Returnval, nil
}

func (m FileManager) MakeDirectory(ctx context.Context, auth types.BaseGuestAuthentication, directoryPath string, createParentDirectories bool) error {
	req := types.MakeDirectoryInGuest{
		This:                    m.Reference(),
		Vm:                      m.vm,
		Auth:                    auth,
		DirectoryPath:           directoryPath,
		CreateParentDirectories: createParentDirectories,
	}

	_, err := methods.MakeDirectoryInGuest(ctx, m.c, &req)
	return err
}

func (m FileManager) MoveDirectory(ctx context.Context, auth types.BaseGuestAuthentication, srcDirectoryPath string, dstDirectoryPath string) error {
	req := types.MoveDirectoryInGuest{
		This:             m.Reference(),
		Vm:               m.vm,
		Auth:             auth,
		SrcDirectoryPath: srcDirectoryPath,
		DstDirectoryPath: dstDirectoryPath,
	}

	_, err := methods.MoveDirectoryInGuest(ctx, m.c, &req)
	return err
}

func (m FileManager) MoveFile(ctx context.Context, auth types.BaseGuestAuthentication, srcFilePath string, dstFilePath string, overwrite bool) error {
	req := types.MoveFileInGuest{
		This:        m.Reference(),
		Vm:          m.vm,
		Auth:        auth,
		SrcFilePath: srcFilePath,
		DstFilePath: dstFilePath,
		Overwrite:   overwrite,
	}

	_, err := methods.MoveFileInGuest(ctx, m.c, &req)
	return err
}
