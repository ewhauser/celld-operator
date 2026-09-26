# v0.0.5 manifests

Copied unchanged from the `v0.0.5` tag (`config/crd/`, `config/manager/operator.yaml`
and `config/rbac/fleet-namespace.yaml`). The `upgrade` suite installs them with the
released v0.0.5 operator image, reproduces a disk retired by its launcher (#60),
and then upgrades to the current build. They are vendored so the suite needs no
git history.
