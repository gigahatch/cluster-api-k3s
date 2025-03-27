# Tilt dev setup

- Install requirements: (https://cluster-api.sigs.k8s.io/developer/core/tilt)
- Install go *1.22*

- clone all provider repos into the same folder: 
- - cluster-api (official)
- - cluster-api-k3s (gigahatch)
- - cluster-api-provider-hetzner (gigahatch)
- - cluster-api-addon-provider-helm (official)
    
- cd to `cluster-api` repo
- setup kind cluster: `./hack/kind-install-for-capd.sh`
- create `tilt-settings.yaml`:
```yaml
# refer to https://cluster-api.sigs.k8s.io/developer/tilt.html for documentation
allowed_contexts:
- kind-capi-test
trigger_mode: manual  # set to auto to enable auto-rebuilding
default_registry: ''  # empty means use local registry 
provider_repos:
- ../cluster-api-k3s  # load k3s as a provider, change to a different path if needed
- ../cluster-api-provider-hetzner
- ../cluster-api-addon-provider-helm
enable_providers:
- docker
- k3s-bootstrap
- k3s-control-plane
- gigahatch-hetzner
- helm
deploy_observability:
- visualizer
kustomize_substitutions:
  # enable some experiment features
  CLUSTER_TOPOLOGY: "true"
  EXP_MACHINE_POOL: "true"
  EXP_CLUSTER_RESOURCE_SET: "true"
  EXP_KUBEADM_BOOTSTRAP_FORMAT_IGNITION: "true"
  EXP_RUNTIME_SDK: "true"
  # add variables for workload cluster template
  KUBERNETES_VERSION: "v1.28.6+k3s2"
  KIND_IMAGE_VERSION: "v1.28.0"
  WORKER_MACHINE_COUNT: "1"
  CONTROL_PLANE_MACHINE_COUNT: "1"
  # Note: kustomize substitutions expects the values to be strings. This can be achieved by wrapping the values in quotation marks.
  # also, can use this to provide credentials
kind_cluster_name: capi-test
extra_args:
  # add extra arguments when launching the binary 
  k3s-bootstrap:
  - --enable-leader-election=false
  k3s-control-plane:
  - --enable-leader-election=false
debug: 
  # enable delve for debugging
  docker:
    continue: true  # change to false if you need the service to be running after the delve has been connected
    port: 30000
    profiler_port: 30001
    metrics_port: 30002
  core:
    continue: true
    port: 31000
    profiler_port: 31001
    metrics_port: 31002
  k3s-bootstrap:
    continue: true
    port: 32000
  k3s-control-plane:
    continue: true
    port: 33000
  gigahatch-hetzner:
    continue: true
    port: 34000
template_dirs:
  # add template for fast workload cluster creation, change to a different path if needed
  # you could also add more templates
  k3s-bootstrap:
  # please run `make generate-e2e-templates` to generate the templates first
  - ../cluster-api-k3s/test/e2e/data/infrastructure-docker
```

