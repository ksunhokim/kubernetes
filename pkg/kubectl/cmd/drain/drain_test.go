/*
Copyright 2015 The Kubernetes Authors.

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

package drain

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	extensionsv1beta1 "k8s.io/api/extensions/v1beta1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/genericclioptions/printers"
	"k8s.io/client-go/rest/fake"
	cmdtesting "k8s.io/kubernetes/pkg/kubectl/cmd/testing"
	cmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"k8s.io/kubernetes/pkg/kubectl/scheme"
)

const (
	EvictionMethod = "Eviction"
	DeleteMethod   = "Delete"
)

var node *corev1.Node
var cordoned_node *corev1.Node

func boolptr(b bool) *bool { return &b }

func TestMain(m *testing.M) {
	// Create a node.
	node = &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "node",
			CreationTimestamp: metav1.Time{Time: time.Now()},
		},
		Status: corev1.NodeStatus{},
	}

	// A copy of the same node, but cordoned.
	cordoned_node = node.DeepCopy()
	cordoned_node.Spec.Unschedulable = true
	os.Exit(m.Run())
}

func TestCordon(t *testing.T) {
	tests := []struct {
		description string
		node        *corev1.Node
		expected    *corev1.Node
		cmd         func(cmdutil.Factory, genericclioptions.IOStreams) *cobra.Command
		arg         string
		expectFatal bool
	}{
		{
			description: "node/node syntax",
			node:        cordoned_node,
			expected:    node,
			cmd:         NewCmdUncordon,
			arg:         "node/node",
			expectFatal: false,
		},
		{
			description: "uncordon for real",
			node:        cordoned_node,
			expected:    node,
			cmd:         NewCmdUncordon,
			arg:         "node",
			expectFatal: false,
		},
		{
			description: "uncordon does nothing",
			node:        node,
			expected:    node,
			cmd:         NewCmdUncordon,
			arg:         "node",
			expectFatal: false,
		},
		{
			description: "cordon does nothing",
			node:        cordoned_node,
			expected:    cordoned_node,
			cmd:         NewCmdCordon,
			arg:         "node",
			expectFatal: false,
		},
		{
			description: "cordon for real",
			node:        node,
			expected:    cordoned_node,
			cmd:         NewCmdCordon,
			arg:         "node",
			expectFatal: false,
		},
		{
			description: "cordon missing node",
			node:        node,
			expected:    node,
			cmd:         NewCmdCordon,
			arg:         "bar",
			expectFatal: true,
		},
		{
			description: "uncordon missing node",
			node:        node,
			expected:    node,
			cmd:         NewCmdUncordon,
			arg:         "bar",
			expectFatal: true,
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			tf := cmdtesting.NewTestFactory()
			defer tf.Cleanup()

			codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
			ns := scheme.Codecs

			new_node := &corev1.Node{}
			updated := false
			tf.Client = &fake.RESTClient{
				GroupVersion:         schema.GroupVersion{Group: "", Version: "v1"},
				NegotiatedSerializer: ns,
				Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
					m := &MyReq{req}
					switch {
					case m.isFor("GET", "/nodes/node"):
						return &http.Response{StatusCode: 200, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, test.node)}, nil
					case m.isFor("GET", "/nodes/bar"):
						return &http.Response{StatusCode: 404, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.StringBody("nope")}, nil
					case m.isFor("PATCH", "/nodes/node"):
						data, err := ioutil.ReadAll(req.Body)
						if err != nil {
							t.Fatalf("%s: unexpected error: %v", test.description, err)
						}
						defer req.Body.Close()
						oldJSON, err := runtime.Encode(codec, node)
						if err != nil {
							t.Fatalf("%s: unexpected error: %v", test.description, err)
						}
						appliedPatch, err := strategicpatch.StrategicMergePatch(oldJSON, data, &corev1.Node{})
						if err != nil {
							t.Fatalf("%s: unexpected error: %v", test.description, err)
						}
						if err := runtime.DecodeInto(codec, appliedPatch, new_node); err != nil {
							t.Fatalf("%s: unexpected error: %v", test.description, err)
						}
						if !reflect.DeepEqual(test.expected.Spec, new_node.Spec) {
							t.Fatalf("%s: expected:\n%v\nsaw:\n%v\n", test.description, test.expected.Spec.Unschedulable, new_node.Spec.Unschedulable)
						}
						updated = true
						return &http.Response{StatusCode: 200, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, new_node)}, nil
					default:
						t.Fatalf("%s: unexpected request: %v %#v\n%#v", test.description, req.Method, req.URL, req)
						return nil, nil
					}
				}),
			}
			tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

			ioStreams, _, _, _ := genericclioptions.NewTestIOStreams()
			cmd := test.cmd(tf, ioStreams)

			saw_fatal := false
			func() {
				defer func() {
					// Recover from the panic below.
					_ = recover()
					// Restore cmdutil behavior
					cmdutil.DefaultBehaviorOnFatal()
				}()
				cmdutil.BehaviorOnFatal(func(e string, code int) {
					saw_fatal = true
					panic(e)
				})
				cmd.SetArgs([]string{test.arg})
				cmd.Execute()
			}()

			if test.expectFatal {
				if !saw_fatal {
					t.Fatalf("%s: unexpected non-error", test.description)
				}
				if updated {
					t.Fatalf("%s: unexpected update", test.description)
				}
			}

			if !test.expectFatal && saw_fatal {
				t.Fatalf("%s: unexpected error", test.description)
			}
			if !reflect.DeepEqual(test.expected.Spec, test.node.Spec) && !updated {
				t.Fatalf("%s: node never updated", test.description)
			}
		})
	}
}

func TestDrain(t *testing.T) {
	labels := make(map[string]string)
	labels["my_key"] = "my_value"

	rc := corev1.ReplicationController{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "rc",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
		},
		Spec: corev1.ReplicationControllerSpec{
			Selector: labels,
		},
	}

	rc_pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "ReplicationController",
					Name:               "rc",
					UID:                "123",
					BlockOwnerDeletion: boolptr(true),
					Controller:         boolptr(true),
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
		},
	}

	ds := extensionsv1beta1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "ds",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
		},
		Spec: extensionsv1beta1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
		},
	}

	ds_pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "extensions/v1beta1",
					Kind:               "DaemonSet",
					Name:               "ds",
					BlockOwnerDeletion: boolptr(true),
					Controller:         boolptr(true),
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
		},
	}

	ds_terminated_pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "extensions/v1beta1",
					Kind:               "DaemonSet",
					Name:               "ds",
					BlockOwnerDeletion: boolptr(true),
					Controller:         boolptr(true),
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
		},
	}

	ds_pod_with_emptyDir := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "extensions/v1beta1",
					Kind:               "DaemonSet",
					Name:               "ds",
					BlockOwnerDeletion: boolptr(true),
					Controller:         boolptr(true),
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
			Volumes: []corev1.Volume{
				{
					Name:         "scratch",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: ""}},
				},
			},
		},
	}

	orphaned_ds_pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
		},
	}

	job := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "job",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
		},
		Spec: batchv1.JobSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
		},
	}

	job_pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "Job",
					Name:               "job",
					BlockOwnerDeletion: boolptr(true),
					Controller:         boolptr(true),
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
			Volumes: []corev1.Volume{
				{
					Name:         "scratch",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: ""}},
				},
			},
		},
	}

	terminated_job_pod_with_local_storage := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "Job",
					Name:               "job",
					BlockOwnerDeletion: boolptr(true),
					Controller:         boolptr(true),
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
			Volumes: []corev1.Volume{
				{
					Name:         "scratch",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: ""}},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
		},
	}

	rs := extensionsv1beta1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "rs",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
		},
		Spec: extensionsv1beta1.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
		},
	}

	rs_pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "ReplicaSet",
					Name:               "rs",
					BlockOwnerDeletion: boolptr(true),
					Controller:         boolptr(true),
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
		},
	}

	naked_pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
		},
	}

	emptydir_pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
			Volumes: []corev1.Volume{
				{
					Name:         "scratch",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: ""}},
				},
			},
		},
	}
	emptydir_terminated_pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bar",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels:            labels,
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
			Volumes: []corev1.Volume{
				{
					Name:         "scratch",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: ""}},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
		},
	}

	tests := []struct {
		description   string
		node          *corev1.Node
		expected      *corev1.Node
		pods          []corev1.Pod
		rcs           []corev1.ReplicationController
		replicaSets   []extensionsv1beta1.ReplicaSet
		args          []string
		expectWarning string
		expectFatal   bool
		expectDelete  bool
	}{
		{
			description:  "RC-managed pod",
			node:         node,
			expected:     cordoned_node,
			pods:         []corev1.Pod{rc_pod},
			rcs:          []corev1.ReplicationController{rc},
			args:         []string{"node"},
			expectFatal:  false,
			expectDelete: true,
		},
		{
			description:  "DS-managed pod",
			node:         node,
			expected:     cordoned_node,
			pods:         []corev1.Pod{ds_pod},
			rcs:          []corev1.ReplicationController{rc},
			args:         []string{"node"},
			expectFatal:  true,
			expectDelete: false,
		},
		{
			description:  "DS-managed terminated pod",
			node:         node,
			expected:     cordoned_node,
			pods:         []corev1.Pod{ds_terminated_pod},
			rcs:          []corev1.ReplicationController{rc},
			args:         []string{"node"},
			expectFatal:  false,
			expectDelete: true,
		},
		{
			description:  "orphaned DS-managed pod",
			node:         node,
			expected:     cordoned_node,
			pods:         []corev1.Pod{orphaned_ds_pod},
			rcs:          []corev1.ReplicationController{},
			args:         []string{"node"},
			expectFatal:  true,
			expectDelete: false,
		},
		{
			description:  "orphaned DS-managed pod with --force",
			node:         node,
			expected:     cordoned_node,
			pods:         []corev1.Pod{orphaned_ds_pod},
			rcs:          []corev1.ReplicationController{},
			args:         []string{"node", "--force"},
			expectFatal:  false,
			expectDelete: true,
		},
		{
			description:  "DS-managed pod with --ignore-daemonsets",
			node:         node,
			expected:     cordoned_node,
			pods:         []corev1.Pod{ds_pod},
			rcs:          []corev1.ReplicationController{rc},
			args:         []string{"node", "--ignore-daemonsets"},
			expectFatal:  false,
			expectDelete: false,
		},
		{
			description:   "DS-managed pod with emptyDir with --ignore-daemonsets",
			node:          node,
			expected:      cordoned_node,
			pods:          []corev1.Pod{ds_pod_with_emptyDir},
			rcs:           []corev1.ReplicationController{rc},
			args:          []string{"node", "--ignore-daemonsets"},
			expectWarning: "WARNING: Ignoring DaemonSet-managed pods: bar",
			expectFatal:   false,
			expectDelete:  false,
		},
		{
			description:  "Job-managed pod with local storage",
			node:         node,
			expected:     cordoned_node,
			pods:         []corev1.Pod{job_pod},
			rcs:          []corev1.ReplicationController{rc},
			args:         []string{"node", "--force", "--delete-local-data=true"},
			expectFatal:  false,
			expectDelete: true,
		},
		{
			description:  "Job-managed terminated pod",
			node:         node,
			expected:     cordoned_node,
			pods:         []corev1.Pod{terminated_job_pod_with_local_storage},
			rcs:          []corev1.ReplicationController{rc},
			args:         []string{"node"},
			expectFatal:  false,
			expectDelete: true,
		},
		{
			description:  "RS-managed pod",
			node:         node,
			expected:     cordoned_node,
			pods:         []corev1.Pod{rs_pod},
			replicaSets:  []extensionsv1beta1.ReplicaSet{rs},
			args:         []string{"node"},
			expectFatal:  false,
			expectDelete: true,
		},
		{
			description:  "naked pod",
			node:         node,
			expected:     cordoned_node,
			pods:         []corev1.Pod{naked_pod},
			rcs:          []corev1.ReplicationController{},
			args:         []string{"node"},
			expectFatal:  true,
			expectDelete: false,
		},
		{
			description:  "naked pod with --force",
			node:         node,
			expected:     cordoned_node,
			pods:         []corev1.Pod{naked_pod},
			rcs:          []corev1.ReplicationController{},
			args:         []string{"node", "--force"},
			expectFatal:  false,
			expectDelete: true,
		},
		{
			description:  "pod with EmptyDir",
			node:         node,
			expected:     cordoned_node,
			pods:         []corev1.Pod{emptydir_pod},
			args:         []string{"node", "--force"},
			expectFatal:  true,
			expectDelete: false,
		},
		{
			description:  "terminated pod with emptyDir",
			node:         node,
			expected:     cordoned_node,
			pods:         []corev1.Pod{emptydir_terminated_pod},
			rcs:          []corev1.ReplicationController{rc},
			args:         []string{"node"},
			expectFatal:  false,
			expectDelete: true,
		},
		{
			description:  "pod with EmptyDir and --delete-local-data",
			node:         node,
			expected:     cordoned_node,
			pods:         []corev1.Pod{emptydir_pod},
			args:         []string{"node", "--force", "--delete-local-data=true"},
			expectFatal:  false,
			expectDelete: true,
		},
		{
			description:  "empty node",
			node:         node,
			expected:     cordoned_node,
			pods:         []corev1.Pod{},
			rcs:          []corev1.ReplicationController{rc},
			args:         []string{"node"},
			expectFatal:  false,
			expectDelete: false,
		},
	}

	testEviction := false
	for i := 0; i < 2; i++ {
		testEviction = !testEviction
		var currMethod string
		if testEviction {
			currMethod = EvictionMethod
		} else {
			currMethod = DeleteMethod
		}
		for _, test := range tests {
			t.Run(test.description, func(t *testing.T) {
				new_node := &corev1.Node{}
				deleted := false
				evicted := false
				tf := cmdtesting.NewTestFactory()
				defer tf.Cleanup()

				codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
				ns := scheme.Codecs

				tf.Client = &fake.RESTClient{
					GroupVersion:         schema.GroupVersion{Group: "", Version: "v1"},
					NegotiatedSerializer: ns,
					Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
						m := &MyReq{req}
						switch {
						case req.Method == "GET" && req.URL.Path == "/api":
							apiVersions := metav1.APIVersions{
								Versions: []string{"v1"},
							}
							return cmdtesting.GenResponseWithJsonEncodedBody(apiVersions)
						case req.Method == "GET" && req.URL.Path == "/apis":
							groupList := metav1.APIGroupList{
								Groups: []metav1.APIGroup{
									{
										Name: "policy",
										PreferredVersion: metav1.GroupVersionForDiscovery{
											GroupVersion: "policy/v1beta1",
										},
									},
								},
							}
							return cmdtesting.GenResponseWithJsonEncodedBody(groupList)
						case req.Method == "GET" && req.URL.Path == "/api/v1":
							resourceList := metav1.APIResourceList{
								GroupVersion: "v1",
							}
							if testEviction {
								resourceList.APIResources = []metav1.APIResource{
									{
										Name: EvictionSubresource,
										Kind: EvictionKind,
									},
								}
							}
							return cmdtesting.GenResponseWithJsonEncodedBody(resourceList)
						case m.isFor("GET", "/nodes/node"):
							return &http.Response{StatusCode: 200, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, test.node)}, nil
						case m.isFor("GET", "/namespaces/default/replicationcontrollers/rc"):
							return &http.Response{StatusCode: 200, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &test.rcs[0])}, nil
						case m.isFor("GET", "/namespaces/default/daemonsets/ds"):
							return &http.Response{StatusCode: 200, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &ds)}, nil
						case m.isFor("GET", "/namespaces/default/daemonsets/missing-ds"):
							return &http.Response{StatusCode: 404, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &extensionsv1beta1.DaemonSet{})}, nil
						case m.isFor("GET", "/namespaces/default/jobs/job"):
							return &http.Response{StatusCode: 200, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &job)}, nil
						case m.isFor("GET", "/namespaces/default/replicasets/rs"):
							return &http.Response{StatusCode: 200, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &test.replicaSets[0])}, nil
						case m.isFor("GET", "/namespaces/default/pods/bar"):
							return &http.Response{StatusCode: 404, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &corev1.Pod{})}, nil
						case m.isFor("GET", "/pods"):
							values, err := url.ParseQuery(req.URL.RawQuery)
							if err != nil {
								t.Fatalf("%s: unexpected error: %v", test.description, err)
							}
							get_params := make(url.Values)
							get_params["fieldSelector"] = []string{"spec.nodeName=node"}
							if !reflect.DeepEqual(get_params, values) {
								t.Fatalf("%s: expected:\n%v\nsaw:\n%v\n", test.description, get_params, values)
							}
							return &http.Response{StatusCode: 200, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &corev1.PodList{Items: test.pods})}, nil
						case m.isFor("GET", "/replicationcontrollers"):
							return &http.Response{StatusCode: 200, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &corev1.ReplicationControllerList{Items: test.rcs})}, nil
						case m.isFor("PATCH", "/nodes/node"):
							data, err := ioutil.ReadAll(req.Body)
							if err != nil {
								t.Fatalf("%s: unexpected error: %v", test.description, err)
							}
							defer req.Body.Close()
							oldJSON, err := runtime.Encode(codec, node)
							if err != nil {
								t.Fatalf("%s: unexpected error: %v", test.description, err)
							}
							appliedPatch, err := strategicpatch.StrategicMergePatch(oldJSON, data, &corev1.Node{})
							if err != nil {
								t.Fatalf("%s: unexpected error: %v", test.description, err)
							}
							if err := runtime.DecodeInto(codec, appliedPatch, new_node); err != nil {
								t.Fatalf("%s: unexpected error: %v", test.description, err)
							}
							if !reflect.DeepEqual(test.expected.Spec, new_node.Spec) {
								t.Fatalf("%s: expected:\n%v\nsaw:\n%v\n", test.description, test.expected.Spec, new_node.Spec)
							}
							return &http.Response{StatusCode: 200, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, new_node)}, nil
						case m.isFor("DELETE", "/namespaces/default/pods/bar"):
							deleted = true
							return &http.Response{StatusCode: 204, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &test.pods[0])}, nil
						case m.isFor("POST", "/namespaces/default/pods/bar/eviction"):
							evicted = true
							return &http.Response{StatusCode: 201, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &policyv1beta1.Eviction{})}, nil
						default:
							t.Fatalf("%s: unexpected request: %v %#v\n%#v", test.description, req.Method, req.URL, req)
							return nil, nil
						}
					}),
				}
				tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

				ioStreams, _, _, errBuf := genericclioptions.NewTestIOStreams()
				cmd := NewCmdDrain(tf, ioStreams)

				saw_fatal := false
				fatal_msg := ""
				func() {
					defer func() {
						// Recover from the panic below.
						_ = recover()
						// Restore cmdutil behavior
						cmdutil.DefaultBehaviorOnFatal()
					}()
					cmdutil.BehaviorOnFatal(func(e string, code int) { saw_fatal = true; fatal_msg = e; panic(e) })
					cmd.SetArgs(test.args)
					cmd.Execute()
				}()
				if test.expectFatal {
					if !saw_fatal {
						t.Fatalf("%s: unexpected non-error when using %s", test.description, currMethod)
					}
				} else {
					if saw_fatal {
						t.Fatalf("%s: unexpected error when using %s: %s", test.description, currMethod, fatal_msg)

					}
				}

				if test.expectDelete {
					// Test Delete
					if !testEviction && !deleted {
						t.Fatalf("%s: pod never deleted", test.description)
					}
					// Test Eviction
					if testEviction && !evicted {
						t.Fatalf("%s: pod never evicted", test.description)
					}
				}
				if !test.expectDelete {
					if deleted {
						t.Fatalf("%s: unexpected delete when using %s", test.description, currMethod)
					}
				}

				if len(test.expectWarning) > 0 {
					if len(errBuf.String()) == 0 {
						t.Fatalf("%s: expected warning, but found no stderr output", test.description)
					}

					// Mac and Bazel on Linux behave differently when returning newlines
					if a, e := errBuf.String(), test.expectWarning; !strings.Contains(a, e) {
						t.Fatalf("%s: actual warning message did not match expected warning message.\n Expecting:\n%v\n  Got:\n%v", test.description, e, a)
					}
				}
			})
		}
	}
}

func TestDeletePods(t *testing.T) {
	ifHasBeenCalled := map[string]bool{}
	tests := []struct {
		description       string
		interval          time.Duration
		timeout           time.Duration
		expectPendingPods bool
		expectError       bool
		expectedError     *error
		getPodFn          func(namespace, name string) (*corev1.Pod, error)
	}{
		{
			description:       "Wait for deleting to complete",
			interval:          100 * time.Millisecond,
			timeout:           10 * time.Second,
			expectPendingPods: false,
			expectError:       false,
			expectedError:     nil,
			getPodFn: func(namespace, name string) (*corev1.Pod, error) {
				oldPodMap, _ := createPods(false)
				newPodMap, _ := createPods(true)
				if oldPod, found := oldPodMap[name]; found {
					if _, ok := ifHasBeenCalled[name]; !ok {
						ifHasBeenCalled[name] = true
						return &oldPod, nil
					}
					if oldPod.ObjectMeta.Generation < 4 {
						newPod := newPodMap[name]
						return &newPod, nil
					}
					return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, name)

				}
				return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, name)
			},
		},
		{
			description:       "Deleting could timeout",
			interval:          200 * time.Millisecond,
			timeout:           3 * time.Second,
			expectPendingPods: true,
			expectError:       true,
			expectedError:     &wait.ErrWaitTimeout,
			getPodFn: func(namespace, name string) (*corev1.Pod, error) {
				oldPodMap, _ := createPods(false)
				if oldPod, found := oldPodMap[name]; found {
					return &oldPod, nil
				}
				return nil, fmt.Errorf("%q: not found", name)
			},
		},
		{
			description:       "Client error could be passed out",
			interval:          200 * time.Millisecond,
			timeout:           5 * time.Second,
			expectPendingPods: true,
			expectError:       true,
			expectedError:     nil,
			getPodFn: func(namespace, name string) (*corev1.Pod, error) {
				return nil, errors.New("this is a random error for testing")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			tf := cmdtesting.NewTestFactory()
			defer tf.Cleanup()

			o := DrainOptions{
				PrintFlags: genericclioptions.NewPrintFlags("drained").WithTypeSetter(scheme.Scheme),
			}
			o.Out = os.Stdout

			o.ToPrinter = func(operation string) (printers.ResourcePrinterFunc, error) {
				return func(obj runtime.Object, out io.Writer) error {
					return nil
				}, nil
			}

			_, pods := createPods(false)
			pendingPods, err := o.waitForDelete(pods, test.interval, test.timeout, false, test.getPodFn)

			if test.expectError {
				if err == nil {
					t.Fatalf("%s: unexpected non-error", test.description)
				} else if test.expectedError != nil {
					if *test.expectedError != err {
						t.Fatalf("%s: the error does not match expected error", test.description)
					}
				}
			}
			if !test.expectError && err != nil {
				t.Fatalf("%s: unexpected error", test.description)
			}
			if test.expectPendingPods && len(pendingPods) == 0 {
				t.Fatalf("%s: unexpected empty pods", test.description)
			}
			if !test.expectPendingPods && len(pendingPods) > 0 {
				t.Fatalf("%s: unexpected pending pods", test.description)
			}
		})
	}
}

func createPods(ifCreateNewPods bool) (map[string]corev1.Pod, []corev1.Pod) {
	podMap := make(map[string]corev1.Pod)
	podSlice := []corev1.Pod{}
	for i := 0; i < 8; i++ {
		var uid types.UID
		if ifCreateNewPods {
			uid = types.UID(i)
		} else {
			uid = types.UID(strconv.Itoa(i) + strconv.Itoa(i))
		}
		pod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "pod" + strconv.Itoa(i),
				Namespace:  "default",
				UID:        uid,
				Generation: int64(i),
			},
		}
		podMap[pod.Name] = pod
		podSlice = append(podSlice, pod)
	}
	return podMap, podSlice
}

type MyReq struct {
	Request *http.Request
}

func (m *MyReq) isFor(method string, path string) bool {
	req := m.Request

	return method == req.Method && (req.URL.Path == path ||
		req.URL.Path == strings.Join([]string{"/api/v1", path}, "") ||
		req.URL.Path == strings.Join([]string{"/apis/extensions/v1beta1", path}, "") ||
		req.URL.Path == strings.Join([]string{"/apis/batch/v1", path}, ""))
}
