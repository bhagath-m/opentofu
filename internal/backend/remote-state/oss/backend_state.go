// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package oss

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"sort"
	"strings"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/aliyun/aliyun-tablestore-go-sdk/tablestore"

	"github.com/opentofu/opentofu/internal/backend"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/opentofu/opentofu/internal/states/remote"
	"github.com/opentofu/opentofu/internal/states/statemgr"
)

const (
	lockFileSuffix = ".tflock"
)

// get a remote client configured for this state
func (b *Backend) remoteClient(name string) (*RemoteClient, error) {
	if name == "" {
		return nil, errors.New("missing state name")
	}

	client := &RemoteClient{
		ossClient:            b.ossClient,
		bucketName:           b.bucketName,
		stateFile:            b.stateFile(name),
		lockFile:             b.lockFile(name),
		serverSideEncryption: b.serverSideEncryption,
		acl:                  b.acl,
		otsTable:             b.otsTable,
		otsClient:            b.otsClient,
	}
	if b.otsEndpoint != "" && b.otsTable != "" {
		_, err := b.otsClient.DescribeTable(&tablestore.DescribeTableRequest{
			TableName: b.otsTable,
		})
		if err != nil {
			return client, fmt.Errorf("error describing table store %s: %w", b.otsTable, err)
		}
	}

	return client, nil
}

func (b *Backend) Workspaces(context.Context) ([]string, error) {
	bucket, err := b.ossClient.Bucket(b.bucketName)
	if err != nil {
		return []string{""}, fmt.Errorf("error getting bucket: %w", err)
	}

	var options []oss.Option
	options = append(options, oss.Prefix(b.statePrefix+"/"), oss.MaxKeys(1000))
	resp, err := bucket.ListObjects(options...)
	if err != nil {
		return nil, err
	}

	result := []string{backend.DefaultStateName}
	prefix := b.statePrefix
	lastObj := ""
	for {
		for _, obj := range resp.Objects {
			// we have 3 parts, the state prefix, the workspace name, and the state file: <prefix>/<workspace-name>/<key>
			if path.Join(b.statePrefix, b.stateKey) == obj.Key {
				// filter the default workspace
				continue
			}
			lastObj = obj.Key
			parts := strings.Split(strings.TrimPrefix(obj.Key, prefix+"/"), "/")
			if len(parts) > 0 && parts[0] != "" {
				result = append(result, parts[0])
			}
		}
		if resp.IsTruncated {
			if len(options) == 3 {
				options[2] = oss.Marker(lastObj)
			} else {
				options = append(options, oss.Marker(lastObj))
			}
			resp, err = bucket.ListObjects(options...)
			if err != nil {
				return nil, err
			}
		} else {
			break
		}
	}
	sort.Strings(result[1:])
	return result, nil
}

func (b *Backend) DeleteWorkspace(ctx context.Context, name string, _ bool) error {
	if name == backend.DefaultStateName || name == "" {
		return fmt.Errorf("can't delete default state")
	}

	client, err := b.remoteClient(name)
	if err != nil {
		return err
	}
	return client.Delete(ctx)
}

func (b *Backend) StateMgr(ctx context.Context, name string) (statemgr.Full, error) {
	client, err := b.remoteClient(name)
	if err != nil {
		return nil, err
	}
	stateMgr := remote.NewState(client, b.encryption)

	// Check to see if this state already exists.
	existing, err := b.Workspaces(ctx)
	if err != nil {
		return nil, err
	}

	log.Printf("[DEBUG] Current workspace name: %s. All workspaces:%#v", name, existing)

	exists := false
	for _, s := range existing {
		if s == name {
			exists = true
			break
		}
	}
	// We need to create the object so it's listed by States.
	if !exists {
		// take a lock on this state while we write it
		lockInfo := statemgr.NewLockInfo()
		lockInfo.Operation = "init"
		lockId, err := client.Lock(context.TODO(), lockInfo)
		if err != nil {
			return nil, fmt.Errorf("failed to lock OSS state: %w", err)
		}

		// Local helper function so we can call it multiple places
		lockUnlock := func(e error) error {
			if err := stateMgr.Unlock(context.TODO(), lockId); err != nil {
				return fmt.Errorf(strings.TrimSpace(stateUnlockError), lockId, err)
			}
			return e
		}

		// Grab the value
		if err := stateMgr.RefreshState(context.TODO()); err != nil {
			err = lockUnlock(err)
			return nil, err
		}

		// If we have no state, we have to create an empty state
		if v := stateMgr.State(); v == nil {
			if err := stateMgr.WriteState(states.NewState()); err != nil {
				err = lockUnlock(err)
				return nil, err
			}
			if err := stateMgr.PersistState(context.TODO(), nil); err != nil {
				err = lockUnlock(err)
				return nil, err
			}
		}

		// Unlock, the state should now be initialized
		if err := lockUnlock(nil); err != nil {
			return nil, err
		}

	}
	return stateMgr, nil
}

func (b *Backend) stateFile(name string) string {
	if name == backend.DefaultStateName {
		return path.Join(b.statePrefix, b.stateKey)
	}
	return path.Join(b.statePrefix, name, b.stateKey)
}

func (b *Backend) lockFile(name string) string {
	return b.stateFile(name) + lockFileSuffix
}

const stateUnlockError = `
Error unlocking Alibaba Cloud OSS state file:

Lock ID: %s
Error message: %w

You may have to force-unlock this state in order to use it again.
The Alibaba Cloud backend acquires a lock during initialization to ensure the initial state file is created.
`
