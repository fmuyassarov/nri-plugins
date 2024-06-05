default_registry('ttl.sh')
allow_k8s_contexts('kubernetes-admin@kubernetes')

# Compile the Go binary locally, and copy the binary to the container.
local_resource(
  'image build',
  'cd cmd/plugins/balloons; CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ../../../build/bin/nri-resource-policy-balloons ./',
  deps=['pkg']) 

# Build: tell Tilt what images to build from which directories
docker_build('ghcr.io/containers/nri-plugins/nri-resource-policy-balloons',
    '.',
    dockerfile='./cmd/plugins/balloons/Dockerfile')

# Deploy the balloons Helm chart
yaml = helm(
  './deployment/helm/balloons',
  # The release name, equivalent to helm --name
  name='balloons',
  # The namespace to install in, equivalent to helm --namespace
  namespace='kube-system',
  )
k8s_yaml(yaml)
