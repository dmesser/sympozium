# Skill sidecar: k8s-ops
# Provides oc, kubectl, helm and supporting tools for Kubernetes/OpenShift skills.
# Uses UBI-minimal (glibc-based) so the oc binary works without shims.
FROM registry.access.redhat.com/ubi9-minimal:latest

ARG KUBECTL_VERSION=v1.31.4
ARG OC_VERSION=4.21.6
ARG HELM_VERSION=v3.17.3
ARG TARGETARCH

RUN microdnf install -y \
    bash \
    tar \
    gzip \
    jq \
    python3 \
    skopeo \
    && microdnf clean all \
    # kubectl
    && curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
       -o /usr/local/bin/kubectl \
    && chmod +x /usr/local/bin/kubectl \
    # oc – download the client matching the cluster version
    && OC_ARCH=$([ "${TARGETARCH}" = "amd64" ] && echo "x86_64" || echo "${TARGETARCH}") \
    && curl -fsSL "https://mirror.openshift.com/pub/openshift-v4/${OC_ARCH}/clients/ocp/${OC_VERSION}/openshift-client-linux-${OC_VERSION}.tar.gz" \
       | tar xz -C /usr/local/bin oc \
    && chmod +x /usr/local/bin/oc \
    # helm
    && curl -fsSL "https://get.helm.sh/helm-${HELM_VERSION}-linux-${TARGETARCH}.tar.gz" \
       | tar xz --strip-components=1 -C /usr/local/bin "linux-${TARGETARCH}/helm" \
    && chmod +x /usr/local/bin/helm

COPY images/skill-k8s-ops/tool-executor.sh /usr/local/bin/tool-executor.sh
COPY images/skill-k8s-ops/acs-scan.sh /usr/local/bin/acs-scan.sh
COPY images/skill-k8s-ops/pyxis-scan.sh /usr/local/bin/pyxis-scan.sh
COPY images/skill-k8s-ops/newer-image.sh /usr/local/bin/newer-image.sh
COPY images/skill-k8s-ops/watch-cr-status.sh /usr/local/bin/watch-cr-status.sh
RUN chmod +x /usr/local/bin/tool-executor.sh /usr/local/bin/acs-scan.sh /usr/local/bin/pyxis-scan.sh /usr/local/bin/newer-image.sh /usr/local/bin/watch-cr-status.sh

USER 1000
WORKDIR /workspace

# The tool executor watches /ipc/tools/ for exec-request JSON files,
# executes them, and writes exec-result JSON files back.
CMD ["/usr/local/bin/tool-executor.sh"]
