/*
Copyright 2016 The Kubernetes Authors.

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

package attachdetach

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	kcache "k8s.io/client-go/tools/cache"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/controller/volume/attachdetach/cache"
	controllervolumetesting "k8s.io/kubernetes/pkg/controller/volume/attachdetach/testing"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/csi"
	"k8s.io/kubernetes/pkg/volume/util"
)

const (
	intreePDUniqueNamePrefix = "kubernetes.io/gce-pd/"
	csiPDUniqueNamePrefix    = "kubernetes.io/csi/pd.csi.storage.gke.io^projects/UNSPECIFIED/zones/UNSPECIFIED/disks/"
)

func Test_NewAttachDetachController_Positive(t *testing.T) {
	// Arrange
	fakeKubeClient := controllervolumetesting.CreateTestClient()
	informerFactory := informers.NewSharedInformerFactory(fakeKubeClient, controller.NoResyncPeriodFunc())

	// Act
	_, err := NewAttachDetachController(
		fakeKubeClient,
		informerFactory.Core().V1().Pods(),
		informerFactory.Core().V1().Nodes(),
		informerFactory.Core().V1().PersistentVolumeClaims(),
		informerFactory.Core().V1().PersistentVolumes(),
		informerFactory.Storage().V1().CSINodes(),
		informerFactory.Storage().V1().CSIDrivers(),
		informerFactory.Storage().V1().VolumeAttachments(),
		nil, /* cloud */
		nil, /* plugins */
		nil, /* prober */
		false,
		5*time.Second,
		DefaultTimerConfig,
		nil, /* filteredDialOptions */
	)

	// Assert
	if err != nil {
		t.Fatalf("Run failed with error. Expected: <no error> Actual: <%v>", err)
	}
}

func Test_AttachDetachControllerStateOfWolrdPopulators_Positive(t *testing.T) {
	// Arrange
	fakeKubeClient := controllervolumetesting.CreateTestClient()
	informerFactory := informers.NewSharedInformerFactory(fakeKubeClient, controller.NoResyncPeriodFunc())
	podInformer := informerFactory.Core().V1().Pods()
	nodeInformer := informerFactory.Core().V1().Nodes()
	pvcInformer := informerFactory.Core().V1().PersistentVolumeClaims()
	pvInformer := informerFactory.Core().V1().PersistentVolumes()
	volumeAttachmentInformer := informerFactory.Storage().V1().VolumeAttachments()

	adc := &attachDetachController{
		kubeClient:             fakeKubeClient,
		pvcLister:              pvcInformer.Lister(),
		pvcsSynced:             pvcInformer.Informer().HasSynced,
		pvLister:               pvInformer.Lister(),
		pvsSynced:              pvInformer.Informer().HasSynced,
		podLister:              podInformer.Lister(),
		podsSynced:             podInformer.Informer().HasSynced,
		nodeLister:             nodeInformer.Lister(),
		nodesSynced:            nodeInformer.Informer().HasSynced,
		volumeAttachmentLister: volumeAttachmentInformer.Lister(),
		volumeAttachmentSynced: volumeAttachmentInformer.Informer().HasSynced,
		cloud:                  nil,
	}

	// Act
	plugins := controllervolumetesting.CreateTestPlugin()
	var prober volume.DynamicPluginProber = nil // TODO (#51147) inject mock

	if err := adc.volumePluginMgr.InitPlugins(plugins, prober, adc); err != nil {
		t.Fatalf("Could not initialize volume plugins for Attach/Detach Controller: %+v", err)
	}

	adc.actualStateOfWorld = cache.NewActualStateOfWorld(&adc.volumePluginMgr)
	adc.desiredStateOfWorld = cache.NewDesiredStateOfWorld(&adc.volumePluginMgr)

	err := adc.populateActualStateOfWorld()
	if err != nil {
		t.Fatalf("Run failed with error. Expected: <no error> Actual: <%v>", err)
	}

	err = adc.populateDesiredStateOfWorld()
	if err != nil {
		t.Fatalf("Run failed with error. Expected: <no error> Actual: %v", err)
	}

	// Test the ActualStateOfWorld contains all the node volumes
	nodes, err := adc.nodeLister.List(labels.Everything())
	if err != nil {
		t.Fatalf("Failed to list nodes in indexer. Expected: <no error> Actual: %v", err)
	}

	for _, node := range nodes {
		nodeName := types.NodeName(node.Name)
		for _, attachedVolume := range node.Status.VolumesAttached {
			attachedState := adc.actualStateOfWorld.GetAttachState(attachedVolume.Name, nodeName)
			if attachedState != cache.AttachStateAttached {
				t.Fatalf("Run failed with error. Node %s, volume %s not found", nodeName, attachedVolume.Name)
			}
		}
	}

	pods, err := adc.podLister.List(labels.Everything())
	if err != nil {
		t.Fatalf("Run failed with error. Expected: <no error> Actual: %v", err)
	}
	for _, pod := range pods {
		uniqueName := fmt.Sprintf("%s/%s", controllervolumetesting.TestPluginName, pod.Spec.Volumes[0].Name)
		nodeName := types.NodeName(pod.Spec.NodeName)
		found := adc.desiredStateOfWorld.VolumeExists(v1.UniqueVolumeName(uniqueName), nodeName)
		if !found {
			t.Fatalf("Run failed with error. Volume %s, node %s not found in DesiredStateOfWorld",
				pod.Spec.Volumes[0].Name,
				pod.Spec.NodeName)
		}
	}
}

func Test_AttachDetachControllerRecovery(t *testing.T) {
	attachDetachRecoveryTestCase(t, []*v1.Pod{}, []*v1.Pod{})
	newPod1 := controllervolumetesting.NewPodWithVolume("newpod-1", "volumeName2", "mynode-1")
	attachDetachRecoveryTestCase(t, []*v1.Pod{newPod1}, []*v1.Pod{})
	newPod1 = controllervolumetesting.NewPodWithVolume("newpod-1", "volumeName2", "mynode-1")
	attachDetachRecoveryTestCase(t, []*v1.Pod{}, []*v1.Pod{newPod1})
	newPod1 = controllervolumetesting.NewPodWithVolume("newpod-1", "volumeName2", "mynode-1")
	newPod2 := controllervolumetesting.NewPodWithVolume("newpod-2", "volumeName3", "mynode-1")
	attachDetachRecoveryTestCase(t, []*v1.Pod{newPod1}, []*v1.Pod{newPod2})
}

func attachDetachRecoveryTestCase(t *testing.T, extraPods1 []*v1.Pod, extraPods2 []*v1.Pod) {
	fakeKubeClient := controllervolumetesting.CreateTestClient()
	informerFactory := informers.NewSharedInformerFactory(fakeKubeClient, time.Second*1)
	//informerFactory := informers.NewSharedInformerFactory(fakeKubeClient, time.Second*1)
	plugins := controllervolumetesting.CreateTestPlugin()
	var prober volume.DynamicPluginProber = nil // TODO (#51147) inject mock
	nodeInformer := informerFactory.Core().V1().Nodes().Informer()
	csiNodeInformer := informerFactory.Storage().V1().CSINodes().Informer()
	podInformer := informerFactory.Core().V1().Pods().Informer()
	var podsNum, extraPodsNum, nodesNum, i int

	// Create the controller
	adcObj, err := NewAttachDetachController(
		fakeKubeClient,
		informerFactory.Core().V1().Pods(),
		informerFactory.Core().V1().Nodes(),
		informerFactory.Core().V1().PersistentVolumeClaims(),
		informerFactory.Core().V1().PersistentVolumes(),
		informerFactory.Storage().V1().CSINodes(),
		informerFactory.Storage().V1().CSIDrivers(),
		informerFactory.Storage().V1().VolumeAttachments(),
		nil, /* cloud */
		plugins,
		prober,
		false,
		1*time.Second,
		DefaultTimerConfig,
		nil, /* filteredDialOptions */
	)

	if err != nil {
		t.Fatalf("Run failed with error. Expected: <no error> Actual: <%v>", err)
	}

	adc := adcObj.(*attachDetachController)

	stopCh := make(chan struct{})

	pods, err := fakeKubeClient.CoreV1().Pods(v1.NamespaceAll).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Run failed with error. Expected: <no error> Actual: %v", err)
	}

	for _, pod := range pods.Items {
		podToAdd := pod
		podInformer.GetIndexer().Add(&podToAdd)
		podsNum++
	}
	nodes, err := fakeKubeClient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Run failed with error. Expected: <no error> Actual: %v", err)
	}
	for _, node := range nodes.Items {
		nodeToAdd := node
		nodeInformer.GetIndexer().Add(&nodeToAdd)
		nodesNum++
	}

	csiNodes, err := fakeKubeClient.StorageV1().CSINodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Run failed with error. Expected: <no error> Actual: %v", err)
	}
	for _, csiNode := range csiNodes.Items {
		csiNodeToAdd := csiNode
		csiNodeInformer.GetIndexer().Add(&csiNodeToAdd)
	}

	informerFactory.Start(stopCh)

	if !kcache.WaitForNamedCacheSync("attach detach", stopCh,
		informerFactory.Core().V1().Pods().Informer().HasSynced,
		informerFactory.Core().V1().Nodes().Informer().HasSynced,
		informerFactory.Storage().V1().CSINodes().Informer().HasSynced) {
		t.Fatalf("Error waiting for the informer caches to sync")
	}

	// Make sure the nodes and pods are in the informer cache
	i = 0
	nodeList, err := informerFactory.Core().V1().Nodes().Lister().List(labels.Everything())
	for len(nodeList) < nodesNum {
		if err != nil {
			t.Fatalf("Error getting list of nodes %v", err)
		}
		if i > 100 {
			t.Fatalf("Time out while waiting for the node informer sync: found %d nodes, expected %d nodes", len(nodeList), nodesNum)
		}
		time.Sleep(100 * time.Millisecond)
		nodeList, err = informerFactory.Core().V1().Nodes().Lister().List(labels.Everything())
		i++
	}
	i = 0
	podList, err := informerFactory.Core().V1().Pods().Lister().List(labels.Everything())
	for len(podList) < podsNum {
		if err != nil {
			t.Fatalf("Error getting list of nodes %v", err)
		}
		if i > 100 {
			t.Fatalf("Time out while waiting for the pod informer sync: found %d pods, expected %d pods", len(podList), podsNum)
		}
		time.Sleep(100 * time.Millisecond)
		podList, err = informerFactory.Core().V1().Pods().Lister().List(labels.Everything())
		i++
	}
	i = 0
	csiNodesList, err := informerFactory.Storage().V1().CSINodes().Lister().List(labels.Everything())
	for len(csiNodesList) < nodesNum {
		if err != nil {
			t.Fatalf("Error getting list of csi nodes %v", err)
		}
		if i > 100 {
			t.Fatalf("Time out while waiting for the csinodes informer sync: found %d csinodes, expected %d csinodes", len(csiNodesList), nodesNum)
		}
		time.Sleep(100 * time.Millisecond)
		csiNodesList, err = informerFactory.Storage().V1().CSINodes().Lister().List(labels.Everything())
		i++
	}

	// Populate ASW
	err = adc.populateActualStateOfWorld()
	if err != nil {
		t.Fatalf("Run failed with error. Expected: <no error> Actual: <%v>", err)
	}

	for _, newPod := range extraPods1 {
		// Add a new pod between ASW and DSW ppoulators
		_, err = adc.kubeClient.CoreV1().Pods(newPod.ObjectMeta.Namespace).Create(context.TODO(), newPod, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Run failed with error. Failed to create a new pod: <%v>", err)
		}
		extraPodsNum++
		podInformer.GetIndexer().Add(newPod)

	}

	// Populate DSW
	err = adc.populateDesiredStateOfWorld()
	if err != nil {
		t.Fatalf("Run failed with error. Expected: <no error> Actual: %v", err)
	}

	for _, newPod := range extraPods2 {
		// Add a new pod between DSW ppoulator and reconciler run
		_, err = adc.kubeClient.CoreV1().Pods(newPod.ObjectMeta.Namespace).Create(context.TODO(), newPod, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Run failed with error. Failed to create a new pod: <%v>", err)
		}
		extraPodsNum++
		podInformer.GetIndexer().Add(newPod)
	}

	go adc.reconciler.Run(stopCh)
	go adc.desiredStateOfWorldPopulator.Run(stopCh)
	defer close(stopCh)

	time.Sleep(time.Second * 1) // Wait so the reconciler calls sync at least once

	testPlugin := plugins[0].(*controllervolumetesting.TestPlugin)
	for i = 0; i <= 10; i++ {
		var attachedVolumesNum int = 0
		var detachedVolumesNum int = 0

		time.Sleep(time.Second * 1) // Wait for a second
		for _, volumeList := range testPlugin.GetAttachedVolumes() {
			attachedVolumesNum += len(volumeList)
		}
		for _, volumeList := range testPlugin.GetDetachedVolumes() {
			detachedVolumesNum += len(volumeList)
		}

		// All the "extra pods" should result in volume to be attached, the pods all share one volume
		// which should be attached (+1), the volumes found only in the nodes status should be detached
		if attachedVolumesNum == 1+extraPodsNum && detachedVolumesNum == nodesNum {
			break
		}
		if i == 10 { // 10 seconds time out
			t.Fatalf("Waiting for the volumes to attach/detach timed out: attached %d (expected %d); detached %d (%d)",
				attachedVolumesNum, 1+extraPodsNum, detachedVolumesNum, nodesNum)
		}
	}

	if testPlugin.GetErrorEncountered() {
		t.Fatalf("Fatal error encountered in the testing volume plugin")
	}

}

type vaTest struct {
	testName               string
	volName                string
	podName                string
	podNodeName            string
	pvName                 string
	vaName                 string
	vaNodeName             string
	vaAttachStatus         bool
	csiMigration           bool
	expected_attaches      map[string][]string
	expected_detaches      map[string][]string
	expectedASWAttachState cache.AttachState
}

func Test_ADC_VolumeAttachmentRecovery(t *testing.T) {
	for _, tc := range []vaTest{
		{ // pod is scheduled
			testName:          "Scheduled pod",
			volName:           "vol1",
			podName:           "pod1",
			podNodeName:       "mynode-1",
			pvName:            "pv1",
			vaName:            "va1",
			vaNodeName:        "mynode-1",
			vaAttachStatus:    false,
			expected_attaches: map[string][]string{"mynode-1": {"vol1"}},
			expected_detaches: map[string][]string{},
		},
		{ // pod is deleted, attach status:true, verify dangling volume is detached
			testName:          "VA status is attached",
			volName:           "vol1",
			pvName:            "pv1",
			vaName:            "va1",
			vaNodeName:        "mynode-1",
			vaAttachStatus:    true,
			expected_attaches: map[string][]string{},
			expected_detaches: map[string][]string{"mynode-1": {"vol1"}},
		},
		{ // pod is deleted, attach status:false, verify dangling volume is detached
			testName:          "VA status is unattached",
			volName:           "vol1",
			pvName:            "pv1",
			vaName:            "va1",
			vaNodeName:        "mynode-1",
			vaAttachStatus:    false,
			expected_attaches: map[string][]string{},
			expected_detaches: map[string][]string{"mynode-1": {"vol1"}},
		},
		{ // pod is scheduled, volume is migrated, attach status:false, verify volume is marked as attached
			testName:               "Scheduled Pod with migrated PV",
			volName:                "vol1",
			podNodeName:            "mynode-1",
			pvName:                 "pv1",
			vaName:                 "va1",
			vaNodeName:             "mynode-1",
			vaAttachStatus:         false,
			csiMigration:           true,
			expectedASWAttachState: cache.AttachStateAttached,
		},
		{ // pod is deleted, volume is migrated, attach status:false, verify volume is marked as uncertain
			testName:               "Deleted Pod with migrated PV",
			volName:                "vol1",
			pvName:                 "pv1",
			vaName:                 "va1",
			vaNodeName:             "mynode-1",
			vaAttachStatus:         false,
			csiMigration:           true,
			expectedASWAttachState: cache.AttachStateUncertain,
		},
	} {
		t.Run(tc.testName, func(t *testing.T) {
			volumeAttachmentRecoveryTestCase(t, tc)
		})
	}
}

func volumeAttachmentRecoveryTestCase(t *testing.T, tc vaTest) {
	fakeKubeClient := controllervolumetesting.CreateTestClient()
	informerFactory := informers.NewSharedInformerFactory(fakeKubeClient, time.Second*1)
	var plugins []volume.VolumePlugin
	if tc.csiMigration {
		defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.CSIMigration, tc.csiMigration)()
		defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.CSIMigrationGCE, tc.csiMigration)()
		defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.InTreePluginGCEUnregister, tc.csiMigration)()

		// if InTreePluginGCEUnregister is enabled, only the CSI plugin is registered but not the in-tree one
		plugins = append(plugins, csi.ProbeVolumePlugins()...)
	} else {
		plugins = controllervolumetesting.CreateTestPlugin()
	}

	// OCP Carry: disable ADCCSIMigrationGCEPD feature in this unit test when necessary.
	// OCP forces CSI migration in ADC "on", this unit test may need it "off".
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ADCCSIMigrationGCEPD, tc.csiMigration)()

	nodeInformer := informerFactory.Core().V1().Nodes().Informer()
	podInformer := informerFactory.Core().V1().Pods().Informer()
	pvInformer := informerFactory.Core().V1().PersistentVolumes().Informer()
	vaInformer := informerFactory.Storage().V1().VolumeAttachments().Informer()

	// Create the controller
	adcObj, err := NewAttachDetachController(
		fakeKubeClient,
		informerFactory.Core().V1().Pods(),
		informerFactory.Core().V1().Nodes(),
		informerFactory.Core().V1().PersistentVolumeClaims(),
		informerFactory.Core().V1().PersistentVolumes(),
		informerFactory.Storage().V1().CSINodes(),
		informerFactory.Storage().V1().CSIDrivers(),
		informerFactory.Storage().V1().VolumeAttachments(),
		nil, /* cloud */
		plugins,
		nil, /* prober */
		false,
		1*time.Second,
		DefaultTimerConfig,
		nil, /* filteredDialOptions */
	)
	if err != nil {
		t.Fatalf("NewAttachDetachController failed with error. Expected: <no error> Actual: <%v>", err)
	}
	adc := adcObj.(*attachDetachController)

	// Add existing objects (created by testplugin) to the respective informers
	pods, err := fakeKubeClient.CoreV1().Pods(v1.NamespaceAll).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Run failed with error. Expected: <no error> Actual: %v", err)
	}
	for _, pod := range pods.Items {
		podToAdd := pod
		podInformer.GetIndexer().Add(&podToAdd)
	}
	nodes, err := fakeKubeClient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Run failed with error. Expected: <no error> Actual: %v", err)
	}
	for _, node := range nodes.Items {
		nodeToAdd := node
		nodeInformer.GetIndexer().Add(&nodeToAdd)
	}

	if tc.csiMigration {
		newNode := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: tc.podNodeName,
				Labels: map[string]string{
					"name": tc.podNodeName,
				},
				Annotations: map[string]string{
					util.ControllerManagedAttachAnnotation: "true",
				},
			},
			Status: v1.NodeStatus{
				VolumesAttached: []v1.AttachedVolume{
					{
						Name:       v1.UniqueVolumeName(csiPDUniqueNamePrefix + tc.volName),
						DevicePath: "fake/path",
					},
				},
			},
		}
		_, err = adc.kubeClient.CoreV1().Nodes().Update(context.TODO(), newNode, metav1.UpdateOptions{})
		if err != nil {
			t.Fatalf("Run failed with error. Failed to create a new pod: <%v>", err)
		}
		nodeInformer.GetIndexer().Add(&newNode)
	}
	// Create and add objects requested by the test
	if tc.podName != "" {
		newPod := controllervolumetesting.NewPodWithVolume(tc.podName, tc.volName, tc.podNodeName)
		_, err = adc.kubeClient.CoreV1().Pods(newPod.ObjectMeta.Namespace).Create(context.TODO(), newPod, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Run failed with error. Failed to create a new pod: <%v>", err)
		}
		podInformer.GetIndexer().Add(newPod)
	}
	if tc.pvName != "" {
		newPv := controllervolumetesting.NewPV(tc.pvName, tc.volName)
		_, err = adc.kubeClient.CoreV1().PersistentVolumes().Create(context.TODO(), newPv, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Run failed with error. Failed to create a new pv: <%v>", err)
		}
		pvInformer.GetIndexer().Add(newPv)
	}
	if tc.vaName != "" {
		newVa := controllervolumetesting.NewVolumeAttachment(tc.vaName, tc.pvName, tc.vaNodeName, tc.vaAttachStatus)
		_, err = adc.kubeClient.StorageV1().VolumeAttachments().Create(context.TODO(), newVa, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Run failed with error. Failed to create a new volumeAttachment: <%v>", err)
		}
		vaInformer.GetIndexer().Add(newVa)
	}

	// Makesure the informer cache is synced
	stopCh := make(chan struct{})
	informerFactory.Start(stopCh)

	if !kcache.WaitForNamedCacheSync("attach detach", stopCh,
		informerFactory.Core().V1().Pods().Informer().HasSynced,
		informerFactory.Core().V1().Nodes().Informer().HasSynced,
		informerFactory.Core().V1().PersistentVolumes().Informer().HasSynced,
		informerFactory.Storage().V1().VolumeAttachments().Informer().HasSynced) {
		t.Fatalf("Error waiting for the informer caches to sync")
	}

	// Populate ASW
	err = adc.populateActualStateOfWorld()
	if err != nil {
		t.Fatalf("Run failed with error. Expected: <no error> Actual: <%v>", err)
	}

	// Populate DSW
	err = adc.populateDesiredStateOfWorld()
	if err != nil {
		t.Fatalf("Run failed with error. Expected: <no error> Actual: %v", err)
	}
	// Run reconciler and DSW populator loops
	go adc.reconciler.Run(stopCh)
	go adc.desiredStateOfWorldPopulator.Run(stopCh)
	defer close(stopCh)

	if tc.csiMigration {
		verifyExpectedVolumeState(t, adc, tc)
	} else {
		// Verify if expected attaches and detaches have happened
		testPlugin := plugins[0].(*controllervolumetesting.TestPlugin)
		verifyAttachDetachCalls(t, testPlugin, tc)
	}

}

func verifyExpectedVolumeState(t *testing.T, adc *attachDetachController, tc vaTest) {
	// Since csi migration is turned on, the attach state for the PV should be in CSI format.
	attachedState := adc.actualStateOfWorld.GetAttachState(
		v1.UniqueVolumeName(csiPDUniqueNamePrefix+tc.volName), types.NodeName(tc.vaNodeName))
	if attachedState != tc.expectedASWAttachState {
		t.Fatalf("Expected attachedState %v, but it is %v", tc.expectedASWAttachState, attachedState)
	}

	// kubernetes.io/gce-pd/<volName> should not be marked when CSI Migration is on
	// so it should be in detach status
	attachedState = adc.actualStateOfWorld.GetAttachState(
		v1.UniqueVolumeName(intreePDUniqueNamePrefix+tc.volName), types.NodeName(tc.vaNodeName))
	if attachedState != cache.AttachStateDetached {
		t.Fatalf("Expected attachedState not to be %v, but it is %v", cache.AttachStateDetached, attachedState)
	}
}

func verifyAttachDetachCalls(t *testing.T, testPlugin *controllervolumetesting.TestPlugin, tc vaTest) {
	for tries := 0; tries <= 10; tries++ { // wait & try few times before failing the test
		expected_op_map := tc.expected_attaches
		plugin_map := testPlugin.GetAttachedVolumes()
		verify_op := "attach"
		volFound, nodeFound := false, false
		for i := 0; i <= 1; i++ { // verify attaches and detaches
			if i == 1 {
				expected_op_map = tc.expected_detaches
				plugin_map = testPlugin.GetDetachedVolumes()
				verify_op = "detach"
			}
			// Verify every (node, volume) in the expected_op_map is in the
			// plugin_map
			for expectedNode, expectedVolumeList := range expected_op_map {
				var volumeList []string
				volumeList, nodeFound = plugin_map[expectedNode]
				if !nodeFound && tries == 10 {
					t.Fatalf("Expected node not found, node:%v, op: %v, tries: %d",
						expectedNode, verify_op, tries)
				}
				for _, expectedVolume := range expectedVolumeList {
					volFound = false
					for _, volume := range volumeList {
						if expectedVolume == volume {
							volFound = true
							break
						}
					}
					if !volFound && tries == 10 {
						t.Fatalf("Expected %v operation not found, node:%v, volume: %v, tries: %d",
							verify_op, expectedNode, expectedVolume, tries)
					}
				}
			}
		}
		if nodeFound && volFound {
			break
		}
		time.Sleep(time.Second * 1)
	}

	if testPlugin.GetErrorEncountered() {
		t.Fatalf("Fatal error encountered in the testing volume plugin")
	}
}
