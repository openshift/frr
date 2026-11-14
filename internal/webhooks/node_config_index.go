// SPDX-License-Identifier:Apache-2.0

package webhooks

import (
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/metallb/frr-k8s/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

// nodeConfigIndex owns memberships, but borrows immutable informer objects.
// Both informer handlers and admission use the same lock so a snapshot never
// observes a partially applied event. No validation runs while holding the lock.
type nodeConfigIndex struct {
	mu        sync.RWMutex
	nodes     map[string]*indexedNode
	nodeNames []string
	configs   map[string]*indexedConfig
}

type indexedNode struct {
	labels     map[string]string
	configKeys []string // sorted namespace/name keys
}

type indexedConfig struct {
	object   *v1beta1.FRRConfiguration
	selector labels.Selector // nil for an invalid selector
	nodes    map[string]struct{}
}

func newNodeConfigIndex() *nodeConfigIndex {
	return &nodeConfigIndex{
		nodes:   map[string]*indexedNode{},
		configs: map[string]*indexedConfig{},
	}
}

func (v *FRRConfigValidator) registerIndexHandlers(nodes, configs cache.Informer) error {
	v.index = newNodeConfigIndex()
	nodeRegistration, err := nodes.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    v.index.upsertNode,
		UpdateFunc: func(_, obj interface{}) { v.index.upsertNode(obj) },
		DeleteFunc: v.index.deleteNode,
	})
	if err != nil {
		return fmt.Errorf("register node index handler: %w", err)
	}
	configRegistration, err := configs.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    v.index.upsertConfig,
		UpdateFunc: func(_, obj interface{}) { v.index.upsertConfig(obj) },
		DeleteFunc: v.index.deleteConfig,
	})
	if err != nil {
		_ = nodes.RemoveEventHandler(nodeRegistration)
		return fmt.Errorf("register config index handler: %w", err)
	}
	// Informer store sync alone does not mean our handlers have processed the
	// initial adds. Registration sync covers both conditions.
	v.cacheSynced = func() bool { return nodeRegistration.HasSynced() && configRegistration.HasSynced() }
	return nil
}

func (idx *nodeConfigIndex) upsertConfig(obj interface{}) {
	cfg := obj.(*v1beta1.FRRConfiguration)
	selector, err := metav1.LabelSelectorAsSelector(&cfg.Spec.NodeSelector)
	if err != nil {
		// Preserve the existing behavior for invalid objects already in storage:
		// retain their identity for update warnings, but do not select any nodes.
		selector = nil
	}
	key := cfg.Namespace + "/" + cfg.Name
	entry := &indexedConfig{object: cfg, selector: selector, nodes: map[string]struct{}{}}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	old := idx.configs[key]
	for name, node := range idx.nodes {
		if selector != nil && selector.Matches(labels.Set(node.labels)) {
			entry.nodes[name] = struct{}{}
			if old == nil {
				node.configKeys = insertKey(node.configKeys, key)
			} else if _, found := old.nodes[name]; !found {
				node.configKeys = insertKey(node.configKeys, key)
			}
		}
	}
	if old != nil {
		for name := range old.nodes {
			if _, found := entry.nodes[name]; !found {
				node := idx.nodes[name]
				node.configKeys = removeKey(node.configKeys, key)
			}
		}
	}
	idx.configs[key] = entry
}

func (idx *nodeConfigIndex) deleteConfig(obj interface{}) {
	key, err := toolscache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if old := idx.configs[key]; old != nil {
		for name := range old.nodes {
			node := idx.nodes[name]
			node.configKeys = removeKey(node.configKeys, key)
		}
		delete(idx.configs, key)
	}
}

func (idx *nodeConfigIndex) upsertNode(obj interface{}) {
	node := obj.(*corev1.Node)
	idx.mu.Lock()
	defer idx.mu.Unlock()
	old := idx.nodes[node.Name]
	if old != nil && maps.Equal(old.labels, node.Labels) {
		return
	}
	entry := &indexedNode{labels: node.Labels}
	for key, cfg := range idx.configs {
		if cfg.selector != nil && cfg.selector.Matches(labels.Set(node.Labels)) {
			entry.configKeys = append(entry.configKeys, key)
			cfg.nodes[node.Name] = struct{}{}
		} else {
			delete(cfg.nodes, node.Name)
		}
	}
	slices.Sort(entry.configKeys)
	idx.nodes[node.Name] = entry
	if old == nil {
		idx.nodeNames = insertKey(idx.nodeNames, node.Name)
	}
}

func (idx *nodeConfigIndex) deleteNode(obj interface{}) {
	name, err := toolscache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if node := idx.nodes[name]; node != nil {
		for _, key := range node.configKeys {
			delete(idx.configs[key].nodes, name)
		}
		delete(idx.nodes, name)
		idx.nodeNames = removeKey(idx.nodeNames, name)
	}
}

// configsPerMatchingNode snapshots only the configurations on selected nodes.
// The returned lists borrow nested informer data; Validate must copy it before
// mutation. The incoming object replaces only its own namespace/name and is last.
func (idx *nodeConfigIndex) configsPerMatchingNode(incoming *v1beta1.FRRConfiguration, selector labels.Selector) ([]nodeAndConfigs, []string) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	key := incoming.Namespace + "/" + incoming.Name
	var groups []nodeAndConfigs
	for _, name := range idx.nodeNames {
		node := idx.nodes[name]
		if !selector.Matches(labels.Set(node.labels)) {
			continue
		}
		configs := &v1beta1.FRRConfigurationList{Items: make([]v1beta1.FRRConfiguration, 0, len(node.configKeys)+1)}
		for _, existing := range node.configKeys {
			if existing != key {
				configs.Items = append(configs.Items, *idx.configs[existing].object)
			}
		}
		configs.Items = append(configs.Items, *incoming)
		groups = append(groups, nodeAndConfigs{name: name, cfgs: configs})
	}
	var warnings []string
	if old := idx.configs[key]; old != nil && len(groups) > 0 {
		warnings = gracefulRestartChangeWarnings(*old.object, *incoming)
	}
	return groups, warnings
}

func insertKey(keys []string, key string) []string {
	i, found := slices.BinarySearch(keys, key)
	if found {
		return keys
	}
	return slices.Insert(keys, i, key)
}

func removeKey(keys []string, key string) []string {
	i, found := slices.BinarySearch(keys, key)
	if !found {
		return keys
	}
	return slices.Delete(keys, i, i+1)
}
