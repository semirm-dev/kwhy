#!/usr/bin/env bash
# Fetches the public Kustomize overlays value/kustomize_accuracy_internal_test.go runs against, at pinned commits (the first
# four from the spike, docs/spikes/kustomize/README.md). Shallow, one commit each, only the listed directories when
# given; the clones are gitignored.
set -euo pipefail
dir="$(dirname "$0")/../testdata/kustomize/public"
mkdir -p "$dir"

fetch() { # name url commit [dir...]
  local d="$dir/$1"
  [ -d "$d/.git" ] && return
  git init -q "$d" && git -C "$d" fetch -q --depth 1 "$2" "$3"
  [ $# -gt 3 ] && git -C "$d" sparse-checkout set "${@:4}"
  git -C "$d" checkout -q FETCH_HEAD
}

fetch podinfo https://github.com/stefanprodan/podinfo f72ffd4ae4a46714908911d9c56f0e5f04be8b4a
fetch flux2-example https://github.com/fluxcd/flux2-kustomize-helm-example 4f1a5b1051b469108d1bdf0ebec6c814427e3834
fetch microservices-demo https://github.com/GoogleCloudPlatform/microservices-demo c7cb6d5bfd6ea3aed768634ebbcb98cb7a523712
fetch argo-cd https://github.com/argoproj/argo-cd 232fa2d123c0cef71de9251860289ee9a150172e
# Added 2026-11-23: components, replacements, a CRD-heavy controller stack, an overlay at the repo root.
fetch metrics-server https://github.com/kubernetes-sigs/metrics-server acd676e54f96285dd4e14707bcec54a726401b74 manifests
fetch kubebuilder https://github.com/kubernetes-sigs/kubebuilder 9d0f6d0959861473730bbd19e9fcf2f47a3a44ae testdata/project-v4/config
fetch cluster-api https://github.com/kubernetes-sigs/cluster-api be789f35c752d73d11b209a32e7054c77b34e06d core/config
fetch gatekeeper https://github.com/open-policy-agent/gatekeeper a2bac802bc01a8a1a7639d7b52050ffa79fc785d config
fetch kube-prometheus https://github.com/prometheus-operator/kube-prometheus eb634f11cb010586b5a79f3c10c4ceedd4494696 manifests
