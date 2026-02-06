package controllers

import "strings"

// isWorkerOnlyRole returns true when the role list only contains kube-node (or is explicitly set to that value),
// meaning the Machine behaves like a pure workload node that should not participate in the init lock/cluster.yml.
func isWorkerOnlyRole(roles []string) bool {
	if len(roles) == 0 {
		return false
	}
	for _, role := range roles {
		role = strings.TrimSpace(role)
		if role == "" {
			continue
		}
		if role != "kube-node" {
			return false
		}
	}
	return true
}

func requiresInitAction(roles []string) bool {
	return hasMasterRole(roles) || hasEtcdRole(roles)
}

func hasMasterRole(roles []string) bool {
	for _, role := range roles {
		if strings.TrimSpace(role) == "kube-master" {
			return true
		}
	}
	return false
}

func hasEtcdRole(roles []string) bool {
	for _, role := range roles {
		if strings.TrimSpace(role) == "etcd" {
			return true
		}
	}
	return false
}
