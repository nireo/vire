# vLLM replicas

The GPU overlay at `deploy/k8s/overlays/gpu` deploys this StatefulSet alongside
the gateway. It serves `qwen2.5-0.5b-instruct` on two single-GPU replicas.
The vLLM image is fixed to v0.30.0 and the model revision is fixed in the
StatefulSet. Change both deliberately when upgrading the serving stack.

The headless Service gives each ready replica a stable origin, for example
`http://vire-vllm-0.vire-vllm-headless.vire.svc.cluster.local:8000`. The
gateway registry names these individual origins. Registering the Service's
shared name would break conversation affinity.

To add a replica, increase the StatefulSet count and wait for its `/health`
probe. Add its individual origin to `deploy/k8s/overlays/gpu/models.json`,
apply the overlay, and check `vire_registry_info` on every gateway pod until
their fingerprints agree.

For planned scale-in, remove the replica's origin from the registry first.
Wait for every gateway to publish the new fingerprint and for that backend's
`vire_backend_requests_in_flight` to reach zero on every gateway. Then reduce
the StatefulSet count. An active stream stays on its selected replica until
it finishes. If a replica fails unexpectedly, gateways mark it unhealthy at
the next probe and choose a healthy destination before dispatching new work;
requests already sent are never replayed.
