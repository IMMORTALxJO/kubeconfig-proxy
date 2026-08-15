#!/usr/bin/env bash

# shellcheck disable=SC2034

# Pinned versions used by the compatibility runners. A profile represents a
# Kubernetes minor release; kind publishes a representative node image for the
# minor rather than every upstream patch release.

kcp_select_cluster_version() {
	local profile="$1"

	case "$profile" in
	1.34)
		KCP_SELECTED_KUBERNETES_VERSION="v1.34.0"
		KCP_SELECTED_KIND_NODE_IMAGE="kindest/node:v1.34.0@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a"
		KCP_SELECTED_KUBERNETES_COMMIT="f28b4c9efbca5c5c0af716d9f2d5702667ee8a45"
		;;
	1.35)
		KCP_SELECTED_KUBERNETES_VERSION="v1.35.0"
		KCP_SELECTED_KIND_NODE_IMAGE="kindest/node:v1.35.0@sha256:4613778f3cfcd10e615029370f5786704559103cf27bef934597ba562b269661"
		KCP_SELECTED_KUBERNETES_COMMIT="66452049f3d692768c39c797b21b793dce80314e"
		;;
	1.36)
		KCP_SELECTED_KUBERNETES_VERSION="v1.36.1"
		KCP_SELECTED_KIND_NODE_IMAGE="kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5"
		KCP_SELECTED_KUBERNETES_COMMIT="756939600b9a7180fc2df6550a4585b638875e67"
		;;
	*)
		printf 'unsupported Kubernetes compatibility profile: %s\n' "$profile" >&2
		return 1
		;;
	esac
}

kcp_select_kubectl_version() {
	local profile="$1"

	case "$profile" in
	1.34) KCP_SELECTED_KUBECTL_VERSION="v1.34.0" ;;
	1.35) KCP_SELECTED_KUBECTL_VERSION="v1.35.0" ;;
	1.36) KCP_SELECTED_KUBECTL_VERSION="v1.36.1" ;;
	*)
		printf 'unsupported kubectl compatibility profile: %s\n' "$profile" >&2
		return 1
		;;
	esac
}
