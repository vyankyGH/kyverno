package policycache

import (
	"sync"

	"github.com/go-logr/logr"
	kyverno "github.com/kyverno/kyverno/pkg/api/kyverno/v1"
	kyvernolister "github.com/kyverno/kyverno/pkg/client/listers/kyverno/v1"
	policy2 "github.com/kyverno/kyverno/pkg/policy"
)

type pMap struct {
	sync.RWMutex

	kindDataMap map[string]map[PolicyType][]string

	// nameCacheMap stores the names of all existing policies in dataMap
	// Policy names are stored as <namespace>/<name>
	nameCacheMap map[PolicyType]map[string]bool

	pLister kyvernolister.ClusterPolicyLister

	// npLister can list/get namespace policy from the shared informer's store
	npLister kyvernolister.PolicyLister
}

// policyCache ...
type policyCache struct {
	pMap
	logr.Logger
}

// Interface ...
type Interface interface {
	Add(policy *kyverno.ClusterPolicy)
	Remove(policy *kyverno.ClusterPolicy)
	Get(pkey PolicyType, kind *string, nspace *string) ([]string, []*kyverno.ClusterPolicy)
}

// newPolicyCache ...
func newPolicyCache(log logr.Logger, pLister kyvernolister.ClusterPolicyLister, npLister kyvernolister.PolicyLister) Interface {
	namesCache := map[PolicyType]map[string]bool{
		Mutate:          make(map[string]bool),
		ValidateEnforce: make(map[string]bool),
		ValidateAudit:   make(map[string]bool),
		Generate:        make(map[string]bool),
	}

	return &policyCache{
		pMap{
			nameCacheMap: namesCache,
			kindDataMap:  make(map[string]map[PolicyType][]string),
			pLister:      pLister,
			npLister:     npLister,
		},
		log,
	}
}

// Add a policy to cache
func (pc *policyCache) Add(policy *kyverno.ClusterPolicy) {
	pc.pMap.add(policy)
	pc.Logger.V(4).Info("policy is added to cache", "name", policy.GetName())
}

// Get the list of matched policies
func (pc *policyCache) Get(pkey PolicyType, kind *string, nspace *string) ([]string, []*kyverno.ClusterPolicy) {
	pname, policy := pc.pMap.get(pkey, kind, nspace)
	return pname, policy
}

// Remove a policy from cache
func (pc *policyCache) Remove(policy *kyverno.ClusterPolicy) {
	pc.pMap.remove(policy)
	pc.Logger.V(4).Info("policy is removed from cache", "name", policy.GetName())
}

func (m *pMap) add(policy *kyverno.ClusterPolicy) {
	m.Lock()
	defer m.Unlock()

	enforcePolicy := policy.Spec.ValidationFailureAction == "enforce"
	mutateMap := m.nameCacheMap[Mutate]
	validateEnforceMap := m.nameCacheMap[ValidateEnforce]
	validateAuditMap := m.nameCacheMap[ValidateAudit]
	generateMap := m.nameCacheMap[Generate]
	var pName = policy.GetName()
	pSpace := policy.GetNamespace()
	isNamespacedPolicy := false
	if pSpace != "" {
		pName = pSpace + "/" + pName
		isNamespacedPolicy = true
		// Initialize Namespace Cache Map
	}
	for _, rule := range policy.Spec.Rules {

		for _, kind := range rule.MatchResources.Kinds {
			_, ok := m.kindDataMap[kind]
			if !ok {
				m.kindDataMap[kind] = make(map[PolicyType][]string)
			}

			if rule.HasMutate() {
				if !mutateMap[kind+"/"+pName] {
					mutateMap[kind+"/"+pName] = true
					if isNamespacedPolicy {
						mutatePolicy := m.kindDataMap[kind][Mutate]
						m.kindDataMap[kind][Mutate] = append(mutatePolicy, pName)
						continue
					}
					mutatePolicy := m.kindDataMap[kind][Mutate]
					m.kindDataMap[kind][Mutate] = append(mutatePolicy, policy.GetName())
				}
				continue
			}
			if rule.HasValidate() {
				if enforcePolicy {
					if !validateEnforceMap[kind+"/"+pName] {
						validateEnforceMap[kind+"/"+pName] = true
						if isNamespacedPolicy {
							validatePolicy := m.kindDataMap[kind][ValidateEnforce]
							m.kindDataMap[kind][ValidateEnforce] = append(validatePolicy, pName)
							continue
						}
						validatePolicy := m.kindDataMap[kind][ValidateEnforce]
						m.kindDataMap[kind][ValidateEnforce] = append(validatePolicy, policy.GetName())
					}
					continue
				}

				// ValidateAudit
				if !validateAuditMap[kind+"/"+pName] {
					validateAuditMap[kind+"/"+pName] = true
					if isNamespacedPolicy {
						validatePolicy := m.kindDataMap[kind][ValidateAudit]
						m.kindDataMap[kind][ValidateAudit] = append(validatePolicy, pName)
						continue
					}
					validatePolicy := m.kindDataMap[kind][ValidateAudit]
					m.kindDataMap[kind][ValidateAudit] = append(validatePolicy, policy.GetName())
				}
				continue
			}

			if rule.HasGenerate() {
				if !generateMap[kind+"/"+pName] {
					generateMap[kind+"/"+pName] = true
					if isNamespacedPolicy {
						generatePolicy := m.kindDataMap[kind][Generate]
						m.kindDataMap[kind][Generate] = append(generatePolicy, pName)
						continue
					}
					generatePolicy := m.kindDataMap[kind][Generate]
					m.kindDataMap[kind][Generate] = append(generatePolicy, policy.GetName())
				}
				continue
			}
		}
	}
	m.nameCacheMap[Mutate] = mutateMap
	m.nameCacheMap[ValidateEnforce] = validateEnforceMap
	m.nameCacheMap[ValidateAudit] = validateAuditMap
	m.nameCacheMap[Generate] = generateMap
}

func (m *pMap) get(key PolicyType, kind *string, nspace *string) (pname []string, allPolicies []*kyverno.ClusterPolicy) {
	m.RLock()
	defer m.RUnlock()
	for _, policyName := range m.kindDataMap[*kind][key] {
		ns, key, isNamespacedPolicy := policy2.ParseNamespacedPolicy(policyName)
		if !isNamespacedPolicy {
			policy, _ := m.pLister.Get(key)
			allPolicies = append(allPolicies, policy)
			pname = append(pname, policyName)
		} else {
			if ns == *nspace {
				nspolicy, _ := m.npLister.Policies(ns).Get(key)
				policy := policy2.ConvertPolicyToClusterPolicy(nspolicy)
				allPolicies = append(allPolicies, policy)
				pname = append(pname, key)
			}

		}
	}
	return pname, allPolicies
}

func (m *pMap) remove(policy *kyverno.ClusterPolicy) {
	m.Lock()
	defer m.Unlock()
	var pName = policy.GetName()
	pSpace := policy.GetNamespace()
	if pSpace != "" {
		pName = pSpace + "/" + pName
	}
	for _, rule := range policy.Spec.Rules {
		for _, kind := range rule.MatchResources.Kinds {

			for _, nameCache := range m.nameCacheMap {
				if _, ok := nameCache[kind+"/"+pName]; ok {
					delete(nameCache, kind+"/"+pName)
				}
			}

		}
	}
}
