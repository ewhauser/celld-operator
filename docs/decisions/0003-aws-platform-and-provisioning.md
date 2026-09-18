# ADR 0003 AWS platform and provisioning boundary

Status: Accepted

Date: 2026-09-18

## Context

A concrete initial environment is needed for durability, scheduling, authentication, and failure tests. Cloud infrastructure provisioning would add a second substantial responsibility to the operator.

## Decision

Target AWS EKS with Amazon S3 and Amazon EBS for the first release.

Users provision Kubernetes node capacity, S3 buckets, IAM roles, and associated infrastructure. The operator manages celld workloads and configures their access through supplied identities and storage references. It does not create or manage Karpenter, Cluster Autoscaler, EKS Auto Mode capacity, buckets, or IAM roles.

The operator may create workload PVCs that use a supplied EBS StorageClass; the cluster's storage provisioner handles the corresponding volumes. EBS CSI installation and cloud permissions remain infrastructure prerequisites.

## Consequences

Unschedulable pods and unavailable storage must appear as capacity or lifecycle blockers. The operator cannot assume node provisioning will complete within its scaling window.

Other Kubernetes environments and S3-compatible stores are outside initial qualification. EKS authentication wiring, StorageClass requirements, and environment checks must be documented during implementation.
