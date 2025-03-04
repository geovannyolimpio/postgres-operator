// Copyright 2023 - 2024 Crunchy Data Solutions, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package standalone_pgadmin

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/crunchydata/postgres-operator/internal/config"
	"github.com/crunchydata/postgres-operator/internal/initialize"
	"github.com/crunchydata/postgres-operator/internal/naming"
	"github.com/crunchydata/postgres-operator/pkg/apis/postgres-operator.crunchydata.com/v1beta1"
)

const (
	configMountPath = "/etc/pgadmin/conf.d"
	configFilePath  = "~postgres-operator/" + settingsConfigMapKey
	clusterFilePath = "~postgres-operator/" + settingsClusterMapKey
	ldapFilePath    = "~postgres-operator/ldap-bind-password"

	// Nothing should be mounted to this location except the script our initContainer writes
	scriptMountPath = "/etc/pgadmin"
)

// pod populates a PodSpec with the container and volumes needed to run pgAdmin.
func pod(
	inPGAdmin *v1beta1.PGAdmin,
	inConfigMap *corev1.ConfigMap,
	outPod *corev1.PodSpec,
	pgAdminVolume *corev1.PersistentVolumeClaim,
) {
	const (
		// config and data volume names
		configVolumeName = "pgadmin-config"
		dataVolumeName   = "pgadmin-data"
		logVolumeName    = "pgadmin-log"
		scriptVolumeName = "pgadmin-config-system"
		tempVolumeName   = "tmp"
	)

	// create the projected volume of config maps for use in
	// 1. dynamic server discovery
	// 2. adding the config variables during pgAdmin startup
	configVolume := corev1.Volume{Name: configVolumeName}
	configVolume.VolumeSource = corev1.VolumeSource{
		Projected: &corev1.ProjectedVolumeSource{
			Sources: podConfigFiles(inConfigMap, *inPGAdmin),
		},
	}

	// create the data volume for the persistent database
	dataVolume := corev1.Volume{Name: dataVolumeName}
	dataVolume.VolumeSource = corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: pgAdminVolume.Name,
			ReadOnly:  false,
		},
	}

	// create the temp volume for logs
	logVolume := corev1.Volume{Name: logVolumeName}
	logVolume.VolumeSource = corev1.VolumeSource{
		EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium: corev1.StorageMediumMemory,
		},
	}

	// Volume used to write a custom config_system.py file in the initContainer
	// which then loads the configs found in the `configVolume`
	scriptVolume := corev1.Volume{Name: scriptVolumeName}
	scriptVolume.VolumeSource = corev1.VolumeSource{
		EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium: corev1.StorageMediumMemory,

			// When this volume is too small, the Pod will be evicted and recreated
			// by the StatefulSet controller.
			// - https://kubernetes.io/docs/concepts/storage/volumes/#emptydir
			// NOTE: tmpfs blocks are PAGE_SIZE, usually 4KiB, and size rounds up.
			SizeLimit: resource.NewQuantity(32<<10, resource.BinarySI),
		},
	}

	// create a temp volume for restart pid/other/debugging use
	// TODO: discuss tmp vol vs. persistent vol
	tmpVolume := corev1.Volume{Name: tempVolumeName}
	tmpVolume.VolumeSource = corev1.VolumeSource{
		EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium: corev1.StorageMediumMemory,
		},
	}

	// pgadmin container
	container := corev1.Container{
		Name:            naming.ContainerPGAdmin,
		Command:         startupScript(inPGAdmin),
		Image:           config.StandalonePGAdminContainerImage(inPGAdmin),
		ImagePullPolicy: inPGAdmin.Spec.ImagePullPolicy,
		Resources:       inPGAdmin.Spec.Resources,
		SecurityContext: initialize.RestrictedSecurityContext(),
		Ports: []corev1.ContainerPort{{
			Name:          naming.PortPGAdmin,
			ContainerPort: int32(pgAdminPort),
			Protocol:      corev1.ProtocolTCP,
		}},
		Env: []corev1.EnvVar{
			{
				Name:  "PGADMIN_SETUP_EMAIL",
				Value: fmt.Sprintf("admin@%s.%s.svc", inPGAdmin.Name, inPGAdmin.Namespace),
			},
			{
				Name: "PGADMIN_SETUP_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: naming.StandalonePGAdmin(inPGAdmin).Name,
					},
					Key: "password",
				}},
			},
			{
				Name:  "PGADMIN_LISTEN_PORT",
				Value: fmt.Sprintf("%d", pgAdminPort),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      configVolumeName,
				MountPath: configMountPath,
				ReadOnly:  true,
			},
			{
				Name:      dataVolumeName,
				MountPath: "/var/lib/pgadmin",
			},
			{
				Name:      logVolumeName,
				MountPath: "/var/log/pgadmin",
			},
			{
				Name:      scriptVolumeName,
				MountPath: scriptMountPath,
				ReadOnly:  true,
			},
			{
				Name:      tempVolumeName,
				MountPath: "/tmp",
			},
		},
	}
	startup := corev1.Container{
		Name:            naming.ContainerPGAdminStartup,
		Command:         startupCommand(),
		Image:           container.Image,
		ImagePullPolicy: container.ImagePullPolicy,
		Resources:       container.Resources,
		SecurityContext: initialize.RestrictedSecurityContext(),
		VolumeMounts: []corev1.VolumeMount{
			// Volume to write a custom `config_system.py` file to.
			{
				Name:      scriptVolumeName,
				MountPath: scriptMountPath,
				ReadOnly:  false,
			},
		},
	}

	// add volumes and containers
	outPod.Volumes = []corev1.Volume{
		configVolume,
		dataVolume,
		logVolume,
		scriptVolume,
		tmpVolume,
	}
	outPod.Containers = []corev1.Container{container}
	outPod.InitContainers = []corev1.Container{startup}
}

// podConfigFiles returns projections of pgAdmin's configuration files to
// include in the configuration volume.
func podConfigFiles(configmap *corev1.ConfigMap, pgadmin v1beta1.PGAdmin) []corev1.VolumeProjection {

	config := append(append([]corev1.VolumeProjection{}, pgadmin.Spec.Config.Files...),
		[]corev1.VolumeProjection{
			{
				ConfigMap: &corev1.ConfigMapProjection{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: configmap.Name,
					},
					Items: []corev1.KeyToPath{
						{
							Key:  settingsConfigMapKey,
							Path: configFilePath,
						},
						{
							Key:  settingsClusterMapKey,
							Path: clusterFilePath,
						},
					},
				},
			},
		}...)

	// To enable LDAP authentication for pgAdmin, various LDAP settings must be configured.
	// While most of the required configuration can be set using the 'settings'
	// feature on the spec (.Spec.UserInterface.PGAdmin.Config.Settings), those
	// values are stored in a ConfigMap in plaintext.
	// As a special case, here we mount a provided Secret containing the LDAP_BIND_PASSWORD
	// for use with the other pgAdmin LDAP configuration.
	// - https://www.pgadmin.org/docs/pgadmin4/latest/config_py.html
	// - https://www.pgadmin.org/docs/pgadmin4/development/enabling_ldap_authentication.html
	if pgadmin.Spec.Config.LDAPBindPassword != nil {
		config = append(config, corev1.VolumeProjection{
			Secret: &corev1.SecretProjection{
				LocalObjectReference: pgadmin.Spec.Config.LDAPBindPassword.LocalObjectReference,
				Optional:             pgadmin.Spec.Config.LDAPBindPassword.Optional,
				Items: []corev1.KeyToPath{
					{
						Key:  pgadmin.Spec.Config.LDAPBindPassword.Key,
						Path: ldapFilePath,
					},
				},
			},
		})
	}

	return config
}

func startupScript(pgadmin *v1beta1.PGAdmin) []string {
	// loadServerCommand is a python command leveraging the pgadmin setup.py script
	// with the `--load-servers` flag to replace the servers registered to the admin user
	// with the contents of the `settingsClusterMapKey` file
	var loadServerCommand = fmt.Sprintf(`python3 ${PGADMIN_DIR}/setup.py --load-servers %s/%s --user %s --replace`,
		configMountPath,
		clusterFilePath,
		fmt.Sprintf("admin@%s.%s.svc", pgadmin.Name, pgadmin.Namespace))

	// This script sets up, starts pgadmin, and runs the `loadServerCommand` to register the discovered servers.
	var startScript = fmt.Sprintf(`
PGADMIN_DIR=/usr/local/lib/python3.11/site-packages/pgadmin4

echo "Running pgAdmin4 Setup"
python3 ${PGADMIN_DIR}/setup.py

echo "Starting pgAdmin4"
PGADMIN4_PIDFILE=/tmp/pgadmin4.pid
pgadmin4 &
echo $! > $PGADMIN4_PIDFILE

%s
`, loadServerCommand)

	// Use a Bash loop to periodically check:
	// 1. the mtime of the mounted configuration volume for shared/discovered servers.
	//   When it changes, reload the shared server configuration.
	// 2. that the pgadmin process is still running on the saved proc id.
	//	 When it isn't, we consider pgadmin stopped.
	//   Restart pgadmin and continue watching.

	// Coreutils `sleep` uses a lot of memory, so the following opens a file
	// descriptor and uses the timeout of the builtin `read` to wait. That same
	// descriptor gets closed and reopened to use the builtin `[ -nt` to check mtimes.
	// - https://unix.stackexchange.com/a/407383
	var reloadScript = fmt.Sprintf(`
exec {fd}<> <(:)
while read -r -t 5 -u "${fd}" || true; do
	if [ "${cluster_file}" -nt "/proc/self/fd/${fd}" ] && %s
	then
		exec {fd}>&- && exec {fd}<> <(:)
		stat --format='Loaded shared servers dated %%y' "${cluster_file}"
	fi
	if [ ! -d /proc/$(cat $PGADMIN4_PIDFILE) ]
	then
		pgadmin4 &
		echo $! > $PGADMIN4_PIDFILE
		echo "Restarting pgAdmin4"
	fi
done
`, loadServerCommand)

	wrapper := `monitor() {` + startScript + reloadScript + `}; export cluster_file="$1"; export -f monitor; exec -a "$0" bash -ceu monitor`

	return []string{"bash", "-ceu", "--", wrapper, "pgadmin", fmt.Sprintf("%s/%s", configMountPath, clusterFilePath)}
}

// startupCommand returns an entrypoint that prepares the filesystem for pgAdmin.
func startupCommand() []string {
	// pgAdmin reads from the `/etc/pgadmin/config_system.py` file during startup
	// after all other config files.
	// - https://github.com/pgadmin-org/pgadmin4/blob/REL-7_7/docs/en_US/config_py.rst
	//
	// This command writes a script in `/etc/pgadmin/config_system.py` that reads from
	// the `pgadmin-settings.json` file and the `ldap-bind-password` file (if it exists)
	// and sets those variables globally. That way those values are available as pgAdmin
	// configurations when pgAdmin starts.
	//
	// Note: All pgAdmin settings are uppercase with underscores, so ignore any keys/names
	// that are not.
	//
	// Note: set pgAdmin's LDAP_BIND_PASSWORD setting from the Secret last
	// in order to overwrite configuration of LDAP_BIND_PASSWORD via ConfigMap JSON.
	const (
		// ldapFilePath is the path for mounting the LDAP Bind Password
		ldapPasswordAbsolutePath = configMountPath + "/" + ldapFilePath

		configSystem = `
import glob, json, re, os
DEFAULT_BINARY_PATHS = {'pg': sorted([''] + glob.glob('/usr/pgsql-*/bin')).pop()}
with open('` + configMountPath + `/` + configFilePath + `') as _f:
    _conf, _data = re.compile(r'[A-Z_0-9]+'), json.load(_f)
    if type(_data) is dict:
        globals().update({k: v for k, v in _data.items() if _conf.fullmatch(k)})
if os.path.isfile('` + ldapPasswordAbsolutePath + `'):
    with open('` + ldapPasswordAbsolutePath + `') as _f:
        LDAP_BIND_PASSWORD = _f.read()
`
	)

	args := []string{strings.TrimLeft(configSystem, "\n")}

	script := strings.Join([]string{
		// Use the initContainer to create this path to avoid the error noted here:
		// - https://github.com/kubernetes/kubernetes/issues/121294
		`mkdir -p /etc/pgadmin/conf.d`,
		// Write the system configuration into a read-only file.
		`(umask a-w && echo "$1" > ` + scriptMountPath + `/config_system.py` + `)`,
	}, "\n")

	return append([]string{"bash", "-ceu", "--", script, "startup"}, args...)
}

// podSecurityContext returns a v1.PodSecurityContext for pgadmin that can write
// to PersistentVolumes.
func podSecurityContext(r *PGAdminReconciler) *corev1.PodSecurityContext {
	podSecurityContext := initialize.PodSecurityContext()

	// TODO (dsessler7): Add ability to add supplemental groups

	// OpenShift assigns a filesystem group based on a SecurityContextConstraint.
	// Otherwise, set a filesystem group so pgAdmin can write to files
	// regardless of the UID or GID of a container.
	// - https://cloud.redhat.com/blog/a-guide-to-openshift-and-uids
	// - https://docs.k8s.io/tasks/configure-pod-container/security-context/
	// - https://docs.openshift.com/container-platform/4.14/authentication/managing-security-context-constraints.html
	if !r.IsOpenShift {
		podSecurityContext.FSGroup = initialize.Int64(2)
	}

	return podSecurityContext
}
