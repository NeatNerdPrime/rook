/*
Copyright 2019 The Rook Authors. All rights reserved.

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

package client

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/coreos/pkg/capnslog"
	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	rookclient "github.com/rook/rook/pkg/client/clientset/versioned/fake"
	"github.com/rook/rook/pkg/client/clientset/versioned/scheme"
	"github.com/rook/rook/pkg/clusterd"
	cephclient "github.com/rook/rook/pkg/daemon/ceph/client"
	"github.com/rook/rook/pkg/operator/k8sutil"
	testop "github.com/rook/rook/pkg/operator/test"
	exectest "github.com/rook/rook/pkg/util/exec/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var (
	namespace        = "rook-ceph"
	name             = "my-user"
	dummyVersionsRaw = `
{
	"mon": {
		"ceph version 20.2.0 (0000000000000000) tentacle (stable)": 3
	}
}`
)

func TestValidateClient(t *testing.T) {
	context := &clusterd.Context{Executor: &exectest.MockExecutor{}}

	// must specify caps
	p := cephv1.CephClient{ObjectMeta: metav1.ObjectMeta{Name: "client1", Namespace: "myns"}}
	err := ValidateClient(context, &p)
	assert.NotNil(t, err)

	// must specify name
	p = cephv1.CephClient{ObjectMeta: metav1.ObjectMeta{Namespace: "myns"}}
	err = ValidateClient(context, &p)
	assert.NotNil(t, err)

	// must specify namespace
	p = cephv1.CephClient{ObjectMeta: metav1.ObjectMeta{Name: "client1"}}
	err = ValidateClient(context, &p)
	assert.NotNil(t, err)

	// succeed with caps properly defined
	p = cephv1.CephClient{ObjectMeta: metav1.ObjectMeta{Name: "client1", Namespace: "myns"}}
	p.Spec.Caps = map[string]string{
		"osd": "allow *",
		"mon": "allow *",
		"mds": "allow *",
	}
	err = ValidateClient(context, &p)
	assert.Nil(t, err)
}

func TestGenerateClient(t *testing.T) {
	p := &cephv1.CephClient{
		ObjectMeta: metav1.ObjectMeta{Name: "client1", Namespace: "myns"},
		Spec: cephv1.ClientSpec{
			Caps: map[string]string{
				"osd": "allow *",
				"mon": "allow rw",
				"mds": "allow rwx",
			},
		},
	}

	client, caps := genClientEntity(p)
	assert.Equal(t, []byte(client), []byte("client.client1"))
	assert.True(t, strings.Contains(strings.Join(caps, " "), "osd allow *"))
	assert.True(t, strings.Contains(strings.Join(caps, " "), "mon allow rw"))
	assert.True(t, strings.Contains(strings.Join(caps, " "), "mds allow rwx"))

	// Fail if caps are empty
	p2 := &cephv1.CephClient{
		ObjectMeta: metav1.ObjectMeta{Name: "client2", Namespace: "myns"},
		Spec: cephv1.ClientSpec{
			Caps: map[string]string{
				"osd": "",
				"mon": "",
			},
		},
	}

	client, _ = genClientEntity(p2)
	assert.Equal(t, []byte(client), []byte("client.client2"))
}

func TestCephClientController(t *testing.T) {
	ctx := context.TODO()
	// Set DEBUG logging
	capnslog.SetGlobalLogLevel(capnslog.DEBUG)
	os.Setenv("ROOK_LOG_LEVEL", "DEBUG")

	//
	// TEST 1 SETUP
	//
	// FAILURE because no CephCluster
	//
	logger.Info("RUN 1")
	var (
		name      = "my-client"
		namespace = "rook-ceph"
	)

	// A Pool resource with metadata and spec.
	cephClient := &cephv1.CephClient{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			UID:        types.UID("c47cac40-9bee-4d52-823b-ccd803ba5bfe"),
			Finalizers: []string{"cephclient.ceph.rook.io"},
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "CephClient",
		},
		Spec: cephv1.ClientSpec{
			Caps: map[string]string{
				"osd": "allow *",
				"mon": "allow *",
			},
		},
		Status: &cephv1.CephClientStatus{
			Phase: "",
		},
	}

	// Objects to track in the fake client.
	object := []runtime.Object{
		cephClient,
	}

	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutput: func(command string, args ...string) (string, error) {
			if args[0] == "status" {
				return `{"fsid":"c47cac40-9bee-4d52-823b-ccd803ba5bfe","health":{"checks":{},"status":"HEALTH_ERR"},"pgmap":{"num_pgs":100,"pgs_by_state":[{"state_name":"active+clean","count":100}]}}`, nil
			}
			if args[0] == "versions" {
				return dummyVersionsRaw, nil
			}

			return "", nil
		},
	}
	c := &clusterd.Context{
		Executor:      executor,
		Clientset:     testop.New(t, 1),
		RookClientset: rookclient.NewSimpleClientset(),
	}

	// Register operator types with the runtime scheme.
	s := scheme.Scheme
	s.AddKnownTypes(cephv1.SchemeGroupVersion, &cephv1.CephClient{}, &cephv1.CephClusterList{})

	// Create a fake client to mock API calls.
	cl := fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(object...).Build()

	// Create a ReconcileCephClient object with the scheme and fake client.
	r := &ReconcileCephClient{
		client:           cl,
		scheme:           s,
		context:          c,
		opManagerContext: ctx,
		recorder:         record.NewFakeRecorder(5),
	}

	// Mock request to simulate Reconcile() being called on an event for a
	// watched resource .
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      name,
			Namespace: namespace,
		},
	}

	res, err := r.Reconcile(ctx, req)
	assert.NoError(t, err)
	assert.True(t, res.Requeue)

	//
	// TEST 2:
	//
	// FAILURE we have a cluster but it's not ready
	//
	logger.Info("RUN 2")
	cephCluster := &cephv1.CephCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      namespace,
			Namespace: namespace,
		},
		Status: cephv1.ClusterStatus{
			Phase: "",
			CephVersion: &cephv1.ClusterVersion{
				Version: "14.2.9-0",
			},
			CephStatus: &cephv1.CephStatus{
				Health: "",
			},
		},
	}

	s.AddKnownTypes(cephv1.SchemeGroupVersion, &cephv1.CephCluster{}, &cephv1.CephClusterList{})

	object = append(object, cephCluster)
	// Create a fake client to mock API calls.
	cl = fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(object...).Build()
	// Create a ReconcileCephClient object with the scheme and fake client.
	r = &ReconcileCephClient{
		client:           cl,
		scheme:           s,
		context:          c,
		opManagerContext: ctx,
		recorder:         record.NewFakeRecorder(5),
	}
	assert.True(t, res.Requeue)

	//
	// TEST 3:
	//
	// SUCCESS! The CephCluster is ready
	//
	logger.Info("RUN 3")
	cephCluster.Status.Phase = cephv1.ConditionReady
	cephCluster.Status.CephStatus.Health = "HEALTH_OK"

	objects := []runtime.Object{
		cephClient,
		cephCluster,
	}
	// Create a fake client to mock API calls.
	cl = fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(objects...).Build()
	c.Client = cl

	executor = &exectest.MockExecutor{
		MockExecuteCommandWithOutput: func(command string, args ...string) (string, error) {
			if args[0] == "status" {
				return `{"fsid":"c47cac40-9bee-4d52-823b-ccd803ba5bfe","health":{"checks":{},"status":"HEALTH_OK"},"pgmap":{"num_pgs":100,"pgs_by_state":[{"state_name":"active+clean","count":100}]}}`, nil
			}
			if args[0] == "auth" && args[1] == "get-or-create-key" {
				return `{"key":"AQCvzWBeIV9lFRAAninzm+8XFxbSfTiPwoX50g=="}`, nil
			}
			if args[0] == "versions" {
				return dummyVersionsRaw, nil
			}

			return "", nil
		},
	}
	c.Executor = executor

	// Mock clusterInfo
	secrets := map[string][]byte{
		"fsid":         []byte(name),
		"mon-secret":   []byte("monsecret"),
		"admin-secret": []byte("adminsecret"),
	}
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rook-ceph-mon",
			Namespace: namespace,
		},
		Data: secrets,
		Type: k8sutil.RookType,
	}
	_, err = c.Clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	assert.NoError(t, err)

	s.AddKnownTypes(cephv1.SchemeGroupVersion, &cephv1.CephBlockPoolList{})
	// Create a ReconcileCephClient object with the scheme and fake client.
	r = &ReconcileCephClient{
		client:           cl,
		scheme:           s,
		context:          c,
		opManagerContext: context.TODO(),
		recorder:         record.NewFakeRecorder(5),
	}

	res, err = r.Reconcile(ctx, req)
	assert.NoError(t, err)
	assert.False(t, res.Requeue)

	err = r.client.Get(context.TODO(), req.NamespacedName, cephClient)
	assert.NoError(t, err)
	assert.Equal(t, cephv1.ConditionReady, cephClient.Status.Phase)
	assert.NotEmpty(t, cephClient.Status.Info["secretName"], cephClient.Status.Info)
	cephClientSecret, err := c.Clientset.CoreV1().Secrets(namespace).Get(ctx, cephClient.Status.Info["secretName"], metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotEmpty(t, cephClientSecret.StringData)
	assert.Contains(t, cephClientSecret.StringData, "userID")
	assert.Contains(t, cephClientSecret.StringData, "userKey")
	assert.Contains(t, cephClientSecret.StringData, "adminID")
	assert.Contains(t, cephClientSecret.StringData, "adminKey")
}

func TestBuildUpdateStatusInfo(t *testing.T) {
	cephClient := &cephv1.CephClient{
		ObjectMeta: metav1.ObjectMeta{
			Name: "client-ocp",
		},
		Spec: cephv1.ClientSpec{},
	}

	statusInfo := generateStatusInfo(cephClient)
	assert.NotEmpty(t, statusInfo["secretName"])
	assert.Equal(t, generateCephUserSecretName(cephClient), statusInfo["secretName"])
}

func TestRemoveSecretUpdateStatusInfo(t *testing.T) {
	cephClient := &cephv1.CephClient{
		ObjectMeta: metav1.ObjectMeta{
			Name: "client-ocp",
		},
		Spec: cephv1.ClientSpec{
			RemoveSecret: true,
		},
	}

	statusInfo := generateStatusInfo(cephClient)
	assert.Empty(t, statusInfo)
}

func TestCustomSecretname(t *testing.T) {
	secretName := "test-secret"
	cephClient := &cephv1.CephClient{
		ObjectMeta: metav1.ObjectMeta{
			Name: "client-ocp",
		},
		Spec: cephv1.ClientSpec{
			SecretName: secretName,
		},
	}

	statusInfo := generateStatusInfo(cephClient)
	assert.NotEmpty(t, statusInfo["secretName"])
	assert.Equal(t, secretName, statusInfo["secretName"])
}

func TestReconcileCephClient_reconcileCephClientSecret(t *testing.T) {
	ownerController := true
	tests := []struct {
		name           string
		removeSecret   bool
		existingSecret *v1.Secret
		expectDelete   bool
		expectCreate   bool
		expectUpdate   bool
		expectError    bool
	}{
		{
			name:         "delete if removeSecret is set and owned by same cephClient",
			removeSecret: true,
			existingSecret: &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-secret",
					Namespace: "rook-ceph",
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "ceph.rook.io/v1",
						Kind:       "CephClient",
						Name:       "test-client",
						Controller: &ownerController,
					}},
				},
			},
			expectDelete: true,
		},
		{
			name:         "skip delete if not owned by the current cephClient",
			removeSecret: true,
			existingSecret: &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "client.test",
					Namespace: "rook-ceph",
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "ceph.rook.io/v1",
						Kind:       "CephClient",
						Name:       "another-client",
						Controller: &ownerController,
					}},
				},
			},
			expectDelete: false,
		},
		{
			name:           "create if secret not found",
			removeSecret:   false,
			existingSecret: nil,
			expectCreate:   true,
		},
		{
			name:         "update the secret if owned by the cephClient",
			removeSecret: false,
			existingSecret: &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-secret",
					Namespace:       "rook-ceph",
					ResourceVersion: "123",
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "ceph.rook.io/v1",
						Kind:       "CephClient",
						Name:       "test-client",
						Controller: &ownerController,
					}},
				},
			},
			expectUpdate: true,
		},
		{
			name:         "error if another ceph client is owned and removeSecret is set",
			removeSecret: false,
			existingSecret: &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-secret",
					Namespace: "rook-ceph",
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "ceph.rook.io/v1",
						Kind:       "CephClient",
						Name:       "another-client",
						Controller: &ownerController,
					}},
				},
			},
			expectError: true,
		},
		{
			name:         "update the secret if owned by another resource",
			removeSecret: false,
			existingSecret: &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-secret",
					Namespace:       "rook-ceph",
					ResourceVersion: "123",
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "ceph.rook.io/v1",
						Kind:       "CephCluster",
						Name:       "test-cluster",
						Controller: &ownerController,
					}},
				},
			},
			expectError: true,
		},
		{
			name:         "update the secret if there is no owner",
			removeSecret: false,
			existingSecret: &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-secret",
					Namespace:       "rook-ceph",
					ResourceVersion: "123",
				},
			},
			expectUpdate: false,
			expectError:  true,
		},
		{
			name:           "secret already deleted and removeSecret is set",
			removeSecret:   true,
			existingSecret: nil,
			expectDelete:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := k8sfake.NewSimpleClientset()
			scheme := runtime.NewScheme()
			assert.NoError(t, cephv1.AddToScheme(scheme))
			r := &ReconcileCephClient{
				context: &clusterd.Context{
					Clientset: client,
				},
				scheme: scheme,
				clusterInfo: &cephclient.ClusterInfo{
					Context: context.TODO(),
				},
			}

			cephClient := &cephv1.CephClient{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-client",
					Namespace: "rook-ceph",
				},
				TypeMeta: metav1.TypeMeta{
					APIVersion: "ceph.rook.io/v1",
					Kind:       "CephClient",
				},
				Spec: cephv1.ClientSpec{
					RemoveSecret: tt.removeSecret,
				},
			}

			secret := &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-secret",
					Namespace: "rook-ceph",
				},
				StringData: map[string]string{"foo": "bar"},
			}

			if tt.existingSecret != nil {
				_, err := client.CoreV1().Secrets("rook-ceph").Create(context.TODO(), tt.existingSecret, metav1.CreateOptions{})
				require.NoError(t, err)
			}

			err := r.reconcileCephClientSecret(cephClient, secret)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			secrets, _ := client.CoreV1().Secrets("rook-ceph").List(context.TODO(), metav1.ListOptions{})
			if tt.expectDelete {
				assert.Empty(t, secrets.Items)
			}
			if tt.expectCreate {
				assert.Len(t, secrets.Items, 1)
			}
			if tt.expectUpdate {
				assert.Equal(t, "bar", secrets.Items[0].StringData["foo"])
			}
		})
	}
}

func TestKeyRotation(t *testing.T) {
	// test key rotation end-to-end

	ctx := context.TODO()
	capnslog.SetGlobalLogLevel(capnslog.DEBUG)
	os.Setenv("ROOK_LOG_LEVEL", "DEBUG")

	// create cephclient for test
	cephClient := &cephv1.CephClient{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test",
			Namespace:  namespace,
			UID:        types.UID("c47cac40-9bee-4d52-823b-ccd803ba5bfe"),
			Finalizers: []string{"cephclient.ceph.rook.io"},
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "CephClient",
		},
		Spec: cephv1.ClientSpec{
			Caps: map[string]string{
				"osd": "allow *",
				"mon": "allow *",
			},
			SecretName: "testSecret",
		},
	}

	cephCluster := &cephv1.CephCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      namespace,
			Namespace: namespace,
		},
		Status: cephv1.ClusterStatus{
			Phase: cephv1.ConditionReady,
			CephStatus: &cephv1.CephStatus{
				Health: "HEALTH_OK",
			},
		},
	}

	// auth rotate returns an array instead of a single object
	rotatedKeyJson := `[{"key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="}]`
	userKey := `{"key":"AQCvzWBeIV9lFRAAninzm+8XFxbSfTiPwoX50g=="}`
	userKeyValue := "AQCvzWBeIV9lFRAAninzm+8XFxbSfTiPwoX50g=="

	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutput: func(command string, args ...string) (string, error) {
			if args[0] == "status" {
				return `{"fsid":"c47cac40-9bee-4d52-823b-ccd803ba5bfe","health":{"checks":{},"status":"HEALTH_OK"},"pgmap":{"num_pgs":100,"pgs_by_state":[{"state_name":"active+clean","count":100}]}}`, nil
			}
			if args[0] == "auth" && args[1] == "rotate" {
				t.Logf("rotating key and returning: %s", rotatedKeyJson)
				return rotatedKeyJson, nil
			}
			if args[0] == "auth" && args[1] == "get-or-create-key" {
				return userKey, nil
			}
			if args[0] == "versions" {
				return dummyVersionsRaw, nil
			}
			return "", nil
		},
	}

	clientset := testop.New(t, 3)
	c := &clusterd.Context{
		Executor:  executor,
		Clientset: clientset,
	}

	// Mock clusterInfo
	secrets := map[string][]byte{
		"fsid":         []byte(name),
		"mon-secret":   []byte("monsecret"),
		"admin-secret": []byte("adminsecret"),
	}
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rook-ceph-mon",
			Namespace: namespace,
		},
		Data: secrets,
		Type: k8sutil.RookType,
	}
	_, err := c.Clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	assert.NoError(t, err)

	// Register operator types with the runtime scheme.
	s := scheme.Scheme
	s.AddKnownTypes(cephv1.SchemeGroupVersion, &cephv1.CephClient{}, &cephv1.CephClusterList{})

	// Create a fake client to mock API calls.
	objects := []runtime.Object{
		cephClient,
		cephCluster,
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(objects...).Build()

	r := &ReconcileCephClient{
		client:           cl,
		scheme:           s,
		context:          c,
		opManagerContext: ctx,
		recorder:         record.NewFakeRecorder(10),
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      cephClient.Name,
			Namespace: namespace,
		},
	}

	// NOTE: these unit subtests are not independent. they share state between tests

	// Rotation is not tested exhaustively for every config. The tests are most concerned with
	// ensuring rotation does happen based on inputs and that outputs are updated as expected, for
	// both brownfield and greenfield cases. Cephx helper functions for determining when to rotate
	// and how to update the status are well tested, so we use good-faith assumption that the
	// reconcile implementation uses those, allowing tests here to focus on only the UX aspects

	t.Run("first reconcile", func(t *testing.T) {
		res, err := r.Reconcile(ctx, req)
		assert.NoError(t, err)
		assert.False(t, res.Requeue)

		cephClient := &cephv1.CephClient{}
		err = cl.Get(ctx, req.NamespacedName, cephClient)
		assert.NoError(t, err)
		assert.Equal(t, uint32(1), cephClient.Status.Cephx.KeyGeneration)
		assert.Equal(t, "20.2.0-0", cephClient.Status.Cephx.KeyCephVersion)

		// get the secret and check the keyring
		secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, "testSecret", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.Contains(t, secret.StringData["userKey"], userKeyValue)
	})

	t.Run("subsequent reconcile - retain cephx status", func(t *testing.T) {
		res, err := r.Reconcile(ctx, req)
		assert.NoError(t, err)
		assert.False(t, res.Requeue)

		cephClient := &cephv1.CephClient{}
		err = cl.Get(ctx, req.NamespacedName, cephClient)
		assert.NoError(t, err)
		assert.Equal(t, uint32(1), cephClient.Status.Cephx.KeyGeneration)
		assert.Equal(t, "20.2.0-0", cephClient.Status.Cephx.KeyCephVersion)

		// get the secret and check the keyring
		secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, "testSecret", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.Contains(t, secret.StringData["userKey"], userKeyValue)
	})

	t.Run("brownfield reconcile - retain unknown cephx status", func(t *testing.T) {
		cephClient := &cephv1.CephClient{}
		err = cl.Get(ctx, req.NamespacedName, cephClient)
		assert.NoError(t, err)
		cephClient.Status.Cephx = cephv1.CephxStatus{}
		err = cl.Update(ctx, cephClient)
		assert.NoError(t, err)

		res, err := r.Reconcile(ctx, req)
		assert.NoError(t, err)
		assert.False(t, res.Requeue)

		err = cl.Get(ctx, req.NamespacedName, cephClient)
		assert.NoError(t, err)
		assert.Equal(t, cephv1.CephxStatus{}, cephClient.Status.Cephx)

		// get the secret and check the keyring
		secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, "testSecret", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.Contains(t, secret.StringData["userKey"], userKeyValue)
	})

	t.Run("rotate key - brownfield unknown status becomes known", func(t *testing.T) {
		cephClient := &cephv1.CephClient{}
		err = cl.Get(ctx, req.NamespacedName, cephClient)
		assert.NoError(t, err)
		cephClient.Spec.Security.CephX = cephv1.CephxConfig{
			KeyRotationPolicy: "KeyGeneration",
			KeyGeneration:     2,
		}
		err = cl.Update(ctx, cephClient)
		assert.NoError(t, err)

		rotatedKeyJson = `[{"key":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=="}]`

		res, err := r.Reconcile(ctx, req)
		assert.NoError(t, err)
		assert.False(t, res.Requeue)

		err = cl.Get(ctx, req.NamespacedName, cephClient)
		assert.NoError(t, err)
		assert.Equal(t, uint32(2), cephClient.Status.Cephx.KeyGeneration)
		assert.Equal(t, "20.2.0-0", cephClient.Status.Cephx.KeyCephVersion)

		// get the secret and check the keyring
		secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, "testSecret", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.Contains(t, secret.StringData["userKey"], "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB==")
	})

	t.Run("brownfield reconcile - no further rotation happens", func(t *testing.T) {
		// if rotation happens when it shouldn't, this will let us know by later comparison
		rotatedKeyJson = `[{"key":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC=="}]`

		res, err := r.Reconcile(ctx, req)
		assert.NoError(t, err)
		assert.False(t, res.Requeue)

		cephClient := &cephv1.CephClient{}
		err = cl.Get(ctx, req.NamespacedName, cephClient)
		assert.NoError(t, err)
		assert.Equal(t, uint32(2), cephClient.Status.Cephx.KeyGeneration)
		assert.Equal(t, "20.2.0-0", cephClient.Status.Cephx.KeyCephVersion)

		// get the secret and check the keyring
		secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, "testSecret", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.NotContains(t, secret.StringData["userKey"], "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC==")
	})

	t.Run("rotate key - cephx status updated", func(t *testing.T) {
		cephClient := &cephv1.CephClient{}
		err = cl.Get(ctx, req.NamespacedName, cephClient)
		assert.NoError(t, err)
		cephClient.Spec.Security.CephX = cephv1.CephxConfig{
			KeyRotationPolicy: "KeyGeneration",
			KeyGeneration:     5,
		}
		err = cl.Update(ctx, cephClient)
		assert.NoError(t, err)

		rotatedKeyJson = `[{"key":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC=="}]`

		res, err := r.Reconcile(ctx, req)
		assert.NoError(t, err)
		assert.False(t, res.Requeue)

		err = cl.Get(ctx, req.NamespacedName, cephClient)
		assert.NoError(t, err)
		assert.Equal(t, uint32(5), cephClient.Status.Cephx.KeyGeneration)
		assert.Equal(t, "20.2.0-0", cephClient.Status.Cephx.KeyCephVersion)

		// get the secret and check the keyring
		secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, "testSecret", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.Contains(t, secret.StringData["userKey"], "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC==")
	})
}
