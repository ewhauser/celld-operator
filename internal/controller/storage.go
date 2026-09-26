package controller

import corev1 "k8s.io/api/core/v1"

func persistentAccessModes(opts Options) []corev1.PersistentVolumeAccessMode {
	if !opts.LocalTest || opts.LocalRWOP {
		return []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}
	}
	return []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
}

func (r *Reconciler) supportedCSI(driver string) bool {
	return driver == "ebs.csi.aws.com" || (r.Options.LocalTest && driver == localCSIDriver)
}
