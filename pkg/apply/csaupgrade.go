package apply

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"reflect"
	"sigs.k8s.io/structured-merge-diff/v4/fieldpath"
)

/*
this is taken from k8s.io/client-go/util/csaupgrade
which was not introduced until k8s.io/client-go v0.26.2 but for
openshift release-4.12 this is based off of v0.25.2. In order
to back-port the changes required to "Deprecate legacy field manager"
that was first made in 4.14 [0] we need to create a local copy
of the csaupgrade utils. That is the only purpose of this new
sub-module.

This module is identical to it's v0.26.2 copy with one exception.
csaManagerNames type is converted from sets.Set to map[string]struct{}.
sets.Set is not available from the apimachinery version we are stuck
to iin 4.12 (v0.25.2) as it was first introduced in v0.26.0 [1].

[0] https://github.com/openshift/cluster-network-operator/pull/1763
[1] https://pkg.go.dev/k8s.io/apimachinery/pkg/util/sets#Set
*/

// Calculates a minimal JSON Patch to send to upgrade managed fields
// See `UpgradeManagedFields` for more information.
//
// obj - Target of the operation which has been managed with CSA in the past
// csaManagerNames - Names of FieldManagers to merge into ssaManagerName
// ssaManagerName - Name of FieldManager to be used for `Apply` operations
//
// Returns non-nil error if there was an error, a JSON patch, or nil bytes if
// there is no work to be done.
func upgradeManagedFieldsPatch(
	obj runtime.Object,
	csaManagerNames map[string]struct{},
	ssaManagerName string) ([]byte, error) {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}

	managedFields := accessor.GetManagedFields()
	filteredManagers := accessor.GetManagedFields()
	for csaManagerName := range csaManagerNames {
		filteredManagers, err = upgradedManagedFields(
			filteredManagers, csaManagerName, ssaManagerName)
		if err != nil {
			return nil, err
		}
	}

	if reflect.DeepEqual(managedFields, filteredManagers) {
		// If the managed fields have not changed from the transformed version,
		// there is no patch to perform
		return nil, nil
	}

	// Create a patch with a diff between old and new objects.
	// Just include all managed fields since that is only thing that will change
	//
	// Also include test for RV to avoid race condition
	jsonPatch := []map[string]interface{}{
		{
			"op":    "replace",
			"path":  "/metadata/managedFields",
			"value": filteredManagers,
		},
		{
			// Use "replace" instead of "test" operation so that etcd rejects with
			// 409 conflict instead of apiserver with an invalid request
			"op":    "replace",
			"path":  "/metadata/resourceVersion",
			"value": accessor.GetResourceVersion(),
		},
	}

	return json.Marshal(jsonPatch)
}

// Returns a copy of the provided managed fields that has been migrated from
// client-side-apply to server-side-apply, or an error if there was an issue
func upgradedManagedFields(
	managedFields []metav1.ManagedFieldsEntry,
	csaManagerName string,
	ssaManagerName string,
) ([]metav1.ManagedFieldsEntry, error) {
	if managedFields == nil {
		return nil, nil
	}

	// Create managed fields clone since we modify the values
	managedFieldsCopy := make([]metav1.ManagedFieldsEntry, len(managedFields))
	if copy(managedFieldsCopy, managedFields) != len(managedFields) {
		return nil, errors.New("failed to copy managed fields")
	}
	managedFields = managedFieldsCopy

	// Locate SSA manager
	replaceIndex, managerExists := findFirstIndex(managedFields,
		func(entry metav1.ManagedFieldsEntry) bool {
			return entry.Manager == ssaManagerName &&
				entry.Operation == metav1.ManagedFieldsOperationApply &&
				entry.Subresource == ""
		})

	if !managerExists {
		// SSA manager does not exist. Find the most recent matching CSA manager,
		// convert it to an SSA manager.
		//
		// (find first index, since managed fields are sorted so that most recent is
		//  first in the list)
		replaceIndex, managerExists = findFirstIndex(managedFields,
			func(entry metav1.ManagedFieldsEntry) bool {
				return entry.Manager == csaManagerName &&
					entry.Operation == metav1.ManagedFieldsOperationUpdate &&
					entry.Subresource == ""
			})

		if !managerExists {
			// There are no CSA managers that need to be converted. Nothing to do
			// Return early
			return managedFields, nil
		}

		// Convert CSA manager into SSA manager
		managedFields[replaceIndex].Operation = metav1.ManagedFieldsOperationApply
		managedFields[replaceIndex].Manager = ssaManagerName
	}
	err := unionManagerIntoIndex(managedFields, replaceIndex, csaManagerName)
	if err != nil {
		return nil, err
	}

	// Create version of managed fields which has no CSA managers with the given name
	filteredManagers := filter(managedFields, func(entry metav1.ManagedFieldsEntry) bool {
		return !(entry.Manager == csaManagerName &&
			entry.Operation == metav1.ManagedFieldsOperationUpdate &&
			entry.Subresource == "")
	})

	return filteredManagers, nil
}

// Locates an Update manager entry named `csaManagerName` with the same APIVersion
// as the manager at the targetIndex. Unions both manager's fields together
// into the manager specified by `targetIndex`. No other managers are modified.
func unionManagerIntoIndex(
	entries []metav1.ManagedFieldsEntry,
	targetIndex int,
	csaManagerName string,
) error {
	ssaManager := entries[targetIndex]

	// find Update manager of same APIVersion, union ssa fields with it.
	// discard all other Update managers of the same name
	csaManagerIndex, csaManagerExists := findFirstIndex(entries,
		func(entry metav1.ManagedFieldsEntry) bool {
			return entry.Manager == csaManagerName &&
				entry.Operation == metav1.ManagedFieldsOperationUpdate &&
				//!TODO: some users may want to migrate subresources.
				// should thread through the args at some point.
				entry.Subresource == "" &&
				entry.APIVersion == ssaManager.APIVersion
		})

	targetFieldSet, err := decodeManagedFieldsEntrySet(ssaManager)
	if err != nil {
		return fmt.Errorf("failed to convert fields to set: %w", err)
	}

	combinedFieldSet := &targetFieldSet

	// Union the csa manager with the existing SSA manager. Do nothing if
	// there was no good candidate found
	if csaManagerExists {
		csaManager := entries[csaManagerIndex]

		csaFieldSet, err := decodeManagedFieldsEntrySet(csaManager)
		if err != nil {
			return fmt.Errorf("failed to convert fields to set: %w", err)
		}

		combinedFieldSet = combinedFieldSet.Union(&csaFieldSet)
	}

	// Encode the fields back to the serialized format
	err = encodeManagedFieldsEntrySet(&entries[targetIndex], *combinedFieldSet)
	if err != nil {
		return fmt.Errorf("failed to encode field set: %w", err)
	}

	return nil
}

func findFirstIndex[T any](
	collection []T,
	predicate func(T) bool,
) (int, bool) {
	for idx, entry := range collection {
		if predicate(entry) {
			return idx, true
		}
	}

	return -1, false
}

func filter[T any](
	collection []T,
	predicate func(T) bool,
) []T {
	result := make([]T, 0, len(collection))

	for _, value := range collection {
		if predicate(value) {
			result = append(result, value)
		}
	}

	if len(result) == 0 {
		return nil
	}

	return result
}

// Included from fieldmanager.internal to avoid dependency cycle
// FieldsToSet creates a set paths from an input trie of fields
func decodeManagedFieldsEntrySet(f metav1.ManagedFieldsEntry) (s fieldpath.Set, err error) {
	err = s.FromJSON(bytes.NewReader(f.FieldsV1.Raw))
	return s, err
}

// SetToFields creates a trie of fields from an input set of paths
func encodeManagedFieldsEntrySet(f *metav1.ManagedFieldsEntry, s fieldpath.Set) (err error) {
	f.FieldsV1.Raw, err = s.ToJSON()
	return err
}