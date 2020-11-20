/*
 * This file is part of the CDI project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2020 Red Hat, Inc.
 *
 */

package v1alpha1

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe("IsWaitForFirstConsumerBeforePopulating on Alpha1", func() {

	DescribeTable("PVC with DV as owner",
		func(volumeClaimPhase corev1.PersistentVolumeClaimPhase, dataVolumePhase DataVolumePhase, expectedResponse bool) {

			dv := newCloneDataVolume("source-dv", "default")
			dv.Status.Phase = dataVolumePhase
			controller := true
			sourcePvc := createPvc("test", "default", volumeClaimPhase)
			sourcePvc.OwnerReferences = append(sourcePvc.OwnerReferences, metav1.OwnerReference{
				Kind:       "DataVolume",
				Controller: &controller,
				Name:       "source-dv",
			})
			res, err := IsWaitForFirstConsumerBeforePopulating(sourcePvc,
				func(name, namespace string) (*DataVolume, error) {
					return dv, nil
				})
			Expect(err).ToNot(HaveOccurred())
			Expect(res).To(BeEquivalentTo(expectedResponse))
		},
		Entry("PVC Pending, dv is in WFFC phase", corev1.ClaimPending, WaitForFirstConsumer, true),
		Entry("PVC Pending, dv is NOT in WFFC phase", corev1.ClaimPending, Pending, false),
		Entry("PVC Bound, phase does not matter", corev1.ClaimBound, PhaseUnset, false),
	)

	It("Should return false if source has no ownerRef", func() {
		sourcePvc := createPvc("test", "default", corev1.ClaimPending)
		res, err := IsWaitForFirstConsumerBeforePopulating(sourcePvc,
			func(name, namespace string) (*DataVolume, error) {
				Fail("getDv should never be executed")
				return nil, nil
			})
		Expect(err).ToNot(HaveOccurred())
		Expect(res).To(BeFalse())
	})
})

func createPvc(name, ns string, claimPhase corev1.PersistentVolumeClaimPhase) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       types.UID(ns + "-" + name),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany, corev1.ReadWriteOnce},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceName(corev1.ResourceStorage): resource.MustParse("1G"),
				},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: claimPhase,
		},
	}
}

func newCloneDataVolume(name string, pvcNamespace string) *DataVolume {
	var annCloneToken = "cdi.kubevirt.io/storage.clone.token"
	return &DataVolume{
		TypeMeta: metav1.TypeMeta{APIVersion: SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: metav1.NamespaceDefault,
			Annotations: map[string]string{
				annCloneToken: "foobar",
			},
		},
		Spec: DataVolumeSpec{
			Source: DataVolumeSource{
				PVC: &DataVolumeSourcePVC{
					Name:      "test",
					Namespace: pvcNamespace,
				},
			},
			PVC: &corev1.PersistentVolumeClaimSpec{},
		},
	}
}