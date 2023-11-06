# nri-plugins-operator
## Build operator image
```sh
make docker-build docker-push IMG="example.com/nri-plugins-operator:v0.0.1"
```

## Run in-cluster operator
```sh
make deploy IMG="example.com/nri-plugins-operator:v0.0.1"
```

## Run out-of-cluster operator
```sh
make install run
```

## Install topology-aware plugin
```sh
kubectl apply -f plugins_v1alpha1_topologyawarepluginconfig.yaml
```

## Install balloon plugin
```sh
kubectl apply -f plugins_v1alpha1_balloonspluginconfig.yaml
```
