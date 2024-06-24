helm-terminate
helm_config=${TEST_DIR}/balloons-isolcpus.cfg helm-launch balloons

vm-command "grep isolcpus=0,1 /proc/cmdline" || {
    if [[ "$distro" == *fedora* ]]; then
        fedora-set-kernel-cmdline "isolcpus=0,1"
    else
        ubuntu-set-kernel-cmdline "isolcpus=0,1"
    fi
    vm-reboot
    sleep 5
    vm-command "grep isolcpus=0,1 /proc/cmdline" || {
        error "failed to set isolcpus kernel commandline parameter"
    }
    vm-command "systemctl restart kubelet"
    sleep 5
    vm-wait-process --timeout 120 kube-apiserver
}

# pod0: runs on system isolated CPUs
CONTCOUNT=2 namespace=default create balloons-busybox
report allowed
verify "cpus['pod0c0'] == {'cpu00', 'cpu01'}"

# pod1: should run on non-isolated CPUs
CONTCOUNT=1 namespace="kube-system" create balloons-busybox
report allowed
verify "cpus['pod1c0'] != {'cpu00', 'cpu01'}"

cleanup() {
    vm-command "kubectl delete pods --all --now"
    if [[ "$distro" == *fedora* ]]; then
        fedora-set-kernel-cmdline "isolcpus="
    else
        ubuntu-set-kernel-cmdline "isolcpus="
    fi
    vm-reboot
    sleep 5
    vm-command "grep isolcpus= /proc/cmdline" || {
        error "failed to unset isolcpus kernel commandline parameter"
    }
    vm-command "systemctl restart kubelet"
    sleep 5
    vm-wait-process --timeout 120 kube-apiserver
    return 0
}
cleanup

helm-terminate
