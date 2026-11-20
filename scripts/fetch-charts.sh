#!/usr/bin/env bash
# Fetches the public charts value/accuracy_test.go runs against, at pinned versions.
# Run once and commit the .tgz files; tests never touch the network.
# Versions are the latest stable on 2026-11-19 (the spike did not record its versions).
set -euo pipefail
cd "$(dirname "$0")/../testdata/charts/public"

pull() { helm pull "$@" --destination .; }

pull podinfo --repo https://stefanprodan.github.io/podinfo --version 6.17.0
pull ingress-nginx --repo https://kubernetes.github.io/ingress-nginx --version 4.15.1
pull cert-manager --repo https://charts.jetstack.io --version v1.21.3
pull grafana --repo https://grafana.github.io/helm-charts --version 10.5.15
pull argo-cd --repo https://argoproj.github.io/argo-helm --version 10.18.2
pull kube-prometheus-stack --repo https://prometheus-community.github.io/helm-charts --version 93.4.1
pull external-dns --repo https://kubernetes-sigs.github.io/external-dns --version 1.23.0
pull oci://registry-1.docker.io/bitnamicharts/postgresql --version 18.12.23
pull oci://registry-1.docker.io/bitnamicharts/redis --version 28.3.12
pull oci://registry-1.docker.io/bitnamicharts/keycloak --version 25.2.0

# Drop the version from the file name so tests don't change on a bump.
for f in *-[0-9v]*.tgz; do mv "$f" "$(echo "$f" | sed -E 's/-v?[0-9][^-]*(-[^.]*)?\.tgz$/.tgz/')"; done

# The previous minor of four charts, for change/accuracy_test.go: upgrading to the version above
# must not predict a false "will fail".
mkdir -p ../prev && cd ../prev
pull ingress-nginx --repo https://kubernetes.github.io/ingress-nginx --version 4.14.5
pull cert-manager --repo https://charts.jetstack.io --version v1.20.5
pull grafana --repo https://grafana.github.io/helm-charts --version 10.4.3
pull argo-cd --repo https://argoproj.github.io/argo-helm --version 10.17.2
for f in *-[0-9v]*.tgz; do mv "$f" "$(echo "$f" | sed -E 's/-v?[0-9][^-]*(-[^.]*)?\.tgz$/.tgz/')"; done
