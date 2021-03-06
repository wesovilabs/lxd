package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/wesovilabs/lxd/lxd/db"
	"github.com/wesovilabs/lxd/shared"
	"github.com/wesovilabs/lxd/shared/logger"

	log "gopkg.in/inconshreveable/log15.v2"
)

/* Patches are one-time actions that are sometimes needed to update
   existing container configuration or move things around on the
   filesystem.

   Those patches are applied at startup time after the database schema
   has been fully updated. Patches can therefore assume a working database.

   At the time the patches are applied, the containers aren't started
   yet and the daemon isn't listening to requests.

   DO NOT use this mechanism for database update. Schema updates must be
   done through the separate schema update mechanism.


   Only append to the patches list, never remove entries and never re-order them.
*/

var patches = []patch{
	{name: "invalid_profile_names", run: patchInvalidProfileNames},
	{name: "leftover_profile_config", run: patchLeftoverProfileConfig},
	{name: "network_permissions", run: patchNetworkPermissions},
	{name: "storage_api", run: patchStorageApi},
	{name: "storage_api_v1", run: patchStorageApiV1},
	{name: "storage_api_dir_cleanup", run: patchStorageApiDirCleanup},
	{name: "storage_api_lvm_keys", run: patchStorageApiLvmKeys},
	{name: "storage_api_keys", run: patchStorageApiKeys},
	{name: "storage_api_update_storage_configs", run: patchStorageApiUpdateStorageConfigs},
	{name: "storage_api_lxd_on_btrfs", run: patchStorageApiLxdOnBtrfs},
	{name: "storage_api_lvm_detect_lv_size", run: patchStorageApiDetectLVSize},
	{name: "storage_api_insert_zfs_driver", run: patchStorageApiInsertZfsDriver},
	{name: "storage_zfs_noauto", run: patchStorageZFSnoauto},
	{name: "storage_zfs_volume_size", run: patchStorageZFSVolumeSize},
	{name: "network_dnsmasq_hosts", run: patchNetworkDnsmasqHosts},
	{name: "storage_api_dir_bind_mount", run: patchStorageApiDirBindMount},
	{name: "fix_uploaded_at", run: patchFixUploadedAt},
	{name: "storage_api_ceph_size_remove", run: patchStorageApiCephSizeRemove},
}

type patch struct {
	name string
	run  func(name string, d *Daemon) error
}

func (p *patch) apply(d *Daemon) error {
	logger.Debugf("Applying patch: %s", p.name)

	err := p.run(p.name, d)
	if err != nil {
		return err
	}

	err = db.PatchesMarkApplied(d.db, p.name)
	if err != nil {
		return err
	}

	return nil
}

// Return the names of all available patches.
func patchesGetNames() []string {
	names := make([]string, len(patches))
	for i, patch := range patches {
		names[i] = patch.name
	}
	return names
}

func patchesApplyAll(d *Daemon) error {
	appliedPatches, err := db.Patches(d.db)
	if err != nil {
		return err
	}

	for _, patch := range patches {
		if shared.StringInSlice(patch.name, appliedPatches) {
			continue
		}

		err := patch.apply(d)
		if err != nil {
			return err
		}
	}

	return nil
}

// Patches begin here
func patchLeftoverProfileConfig(name string, d *Daemon) error {
	stmt := `
DELETE FROM profiles_config WHERE profile_id NOT IN (SELECT id FROM profiles);
DELETE FROM profiles_devices WHERE profile_id NOT IN (SELECT id FROM profiles);
DELETE FROM profiles_devices_config WHERE profile_device_id NOT IN (SELECT id FROM profiles_devices);
`

	_, err := d.db.Exec(stmt)
	if err != nil {
		return err
	}

	return nil
}

func patchInvalidProfileNames(name string, d *Daemon) error {
	profiles, err := db.Profiles(d.db)
	if err != nil {
		return err
	}

	for _, profile := range profiles {
		if strings.Contains(profile, "/") || shared.StringInSlice(profile, []string{".", ".."}) {
			logger.Info("Removing unreachable profile (invalid name)", log.Ctx{"name": profile})
			err := db.ProfileDelete(d.db, profile)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func patchNetworkPermissions(name string, d *Daemon) error {
	// Get the list of networks
	networks, err := db.Networks(d.db)
	if err != nil {
		return err
	}

	// Fix the permissions
	err = os.Chmod(shared.VarPath("networks"), 0711)
	if err != nil {
		return err
	}

	for _, network := range networks {
		if !shared.PathExists(shared.VarPath("networks", network)) {
			continue
		}

		err = os.Chmod(shared.VarPath("networks", network), 0711)
		if err != nil {
			return err
		}

		if shared.PathExists(shared.VarPath("networks", network, "dnsmasq.hosts")) {
			err = os.Chmod(shared.VarPath("networks", network, "dnsmasq.hosts"), 0644)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func patchStorageApi(name string, d *Daemon) error {
	lvmVgName := daemonConfig["storage.lvm_vg_name"].Get()
	zfsPoolName := daemonConfig["storage.zfs_pool_name"].Get()
	defaultPoolName := "default"
	preStorageApiStorageType := storageTypeDir

	if lvmVgName != "" {
		preStorageApiStorageType = storageTypeLvm
		defaultPoolName = lvmVgName
	} else if zfsPoolName != "" {
		preStorageApiStorageType = storageTypeZfs
		defaultPoolName = zfsPoolName
	} else if d.os.BackingFS == "btrfs" {
		preStorageApiStorageType = storageTypeBtrfs
	} else {
		// Dir storage pool.
	}

	defaultStorageTypeName, err := storageTypeToString(preStorageApiStorageType)
	if err != nil {
		return err
	}

	// In case we detect that an lvm name or a zfs name exists it makes
	// sense to create a storage pool in the database, independent of
	// whether anything currently exists on that pool. We can still probably
	// safely assume that the user at least once used that pool.
	// However, when we detect {dir, btrfs}, we can't rely on that guess
	// since the daemon doesn't record any name for the pool anywhere.  So
	// in the {dir, btrfs} case we check whether anything exists on the
	// pool, if not, then we don't create a default pool. The user will then
	// be forced to run lxd init again and can start from a pristine state.
	// Check if this LXD instace currently has any containers, snapshots, or
	// images configured. If so, we create a default storage pool in the
	// database. Otherwise, the user will have to run LXD init.
	cRegular, err := db.ContainersList(d.db, db.CTypeRegular)
	if err != nil {
		return err
	}

	// Get list of existing snapshots.
	cSnapshots, err := db.ContainersList(d.db, db.CTypeSnapshot)
	if err != nil {
		return err
	}

	// Get list of existing public images.
	imgPublic, err := db.ImagesGet(d.db, true)
	if err != nil {
		return err
	}

	// Get list of existing private images.
	imgPrivate, err := db.ImagesGet(d.db, false)
	if err != nil {
		return err
	}

	// Nothing exists on the pool so we're not creating a default one,
	// thereby forcing the user to run lxd init.
	if len(cRegular) == 0 && len(cSnapshots) == 0 && len(imgPublic) == 0 && len(imgPrivate) == 0 {
		return nil
	}

	// If any of these are actually called, there's no way back.
	poolName := defaultPoolName
	switch preStorageApiStorageType {
	case storageTypeBtrfs:
		err = upgradeFromStorageTypeBtrfs(name, d, defaultPoolName, defaultStorageTypeName, cRegular, cSnapshots, imgPublic, imgPrivate)
	case storageTypeDir:
		err = upgradeFromStorageTypeDir(name, d, defaultPoolName, defaultStorageTypeName, cRegular, cSnapshots, imgPublic, imgPrivate)
	case storageTypeLvm:
		err = upgradeFromStorageTypeLvm(name, d, defaultPoolName, defaultStorageTypeName, cRegular, cSnapshots, imgPublic, imgPrivate)
	case storageTypeZfs:
		// The user is using a zfs dataset. This case needs to be
		// handled with care:

		// - The pool name that is used in the storage backends needs
		//   to be set to a sane name that doesn't contain a slash "/".
		//   This is what this snippet is for.
		// - The full dataset name <pool_name>/<volume_name> needs to be
		//   set as the source value.
		if strings.Contains(defaultPoolName, "/") {
			poolName = "default"
		}
		err = upgradeFromStorageTypeZfs(name, d, defaultPoolName, defaultStorageTypeName, cRegular, []string{}, imgPublic, imgPrivate)
	default: // Shouldn't happen.
		return fmt.Errorf("Invalid storage type. Upgrading not possible.")
	}
	if err != nil {
		return err
	}

	// The new storage api enforces that the default storage pool on which
	// containers are created is set in the default profile. If it isn't
	// set, then LXD will refuse to create a container until either an
	// appropriate device including a pool is added to the default profile
	// or the user explicitly passes the pool the container's storage volume
	// is supposed to be created on.
	allcontainers := append(cRegular, cSnapshots...)
	err = updatePoolPropertyForAllObjects(d, poolName, allcontainers)
	if err != nil {
		return err
	}

	// Unset deprecated storage keys.
	daemonConfig["storage.lvm_fstype"].Set(d, "")
	daemonConfig["storage.lvm_mount_options"].Set(d, "")
	daemonConfig["storage.lvm_thinpool_name"].Set(d, "")
	daemonConfig["storage.lvm_vg_name"].Set(d, "")
	daemonConfig["storage.lvm_volume_size"].Set(d, "")
	daemonConfig["storage.zfs_pool_name"].Set(d, "")
	daemonConfig["storage.zfs_remove_snapshots"].Set(d, "")
	daemonConfig["storage.zfs_use_refquota"].Set(d, "")

	return SetupStorageDriver(d.State(), true)
}

func upgradeFromStorageTypeBtrfs(name string, d *Daemon, defaultPoolName string, defaultStorageTypeName string, cRegular []string, cSnapshots []string, imgPublic []string, imgPrivate []string) error {
	poolConfig := map[string]string{}
	poolSubvolumePath := getStoragePoolMountPoint(defaultPoolName)
	poolConfig["source"] = poolSubvolumePath

	err := storagePoolValidateConfig(defaultPoolName, defaultStorageTypeName, poolConfig, nil)
	if err != nil {
		return err
	}

	err = storagePoolFillDefault(defaultPoolName, defaultStorageTypeName, poolConfig)
	if err != nil {
		return err
	}

	poolID := int64(-1)
	pools, err := db.StoragePools(d.db)
	if err == nil { // Already exist valid storage pools.
		// Check if the storage pool already has a db entry.
		if shared.StringInSlice(defaultPoolName, pools) {
			logger.Warnf("Database already contains a valid entry for the storage pool: %s.", defaultPoolName)
		}

		// Get the pool ID as we need it for storage volume creation.
		// (Use a tmp variable as Go's scoping is freaking me out.)
		tmp, pool, err := db.StoragePoolGet(d.db, defaultPoolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s.", err)
			return err
		}
		poolID = tmp

		// Update the pool configuration on a post LXD 2.9.1 instance
		// that still runs this upgrade code because of a partial
		// upgrade.
		if pool.Config == nil {
			pool.Config = poolConfig
		}
		err = db.StoragePoolUpdate(d.db, defaultPoolName, "", pool.Config)
		if err != nil {
			return err
		}
	} else if err == db.NoSuchObjectError { // Likely a pristine upgrade.
		tmp, err := dbStoragePoolCreateAndUpdateCache(d.db, defaultPoolName, "", defaultStorageTypeName, poolConfig)
		if err != nil {
			return err
		}
		poolID = tmp

		s, err := storagePoolInit(d.State(), defaultPoolName)
		if err != nil {
			return err
		}

		err = s.StoragePoolCreate()
		if err != nil {
			return err
		}
	} else { // Shouldn't happen.
		logger.Errorf("Failed to query database: %s.", err)
		return err
	}

	if len(cRegular) > 0 {
		// ${LXD_DIR}/storage-pools/<name>
		containersSubvolumePath := getContainerMountPoint(defaultPoolName, "")
		if !shared.PathExists(containersSubvolumePath) {
			err := os.MkdirAll(containersSubvolumePath, 0711)
			if err != nil {
				return err
			}
		}
	}

	// Get storage pool from the db after having updated it above.
	_, defaultPool, err := db.StoragePoolGet(d.db, defaultPoolName)
	if err != nil {
		return err
	}

	for _, ct := range cRegular {
		// Initialize empty storage volume configuration for the
		// container.
		containerPoolVolumeConfig := map[string]string{}
		err = storageVolumeFillDefault(ct, containerPoolVolumeConfig, defaultPool)
		if err != nil {
			return err
		}

		_, err = db.StoragePoolVolumeGetTypeID(d.db, ct, storagePoolVolumeTypeContainer, poolID)
		if err == nil {
			logger.Warnf("Storage volumes database already contains an entry for the container.")
			err := db.StoragePoolVolumeUpdate(d.db, ct, storagePoolVolumeTypeContainer, poolID, "", containerPoolVolumeConfig)
			if err != nil {
				return err
			}
		} else if err == db.NoSuchObjectError {
			// Insert storage volumes for containers into the database.
			_, err := db.StoragePoolVolumeCreate(d.db, ct, "", storagePoolVolumeTypeContainer, poolID, containerPoolVolumeConfig)
			if err != nil {
				logger.Errorf("Could not insert a storage volume for container \"%s\".", ct)
				return err
			}
		} else {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// Rename the btrfs subvolume and making it a
		// subvolume of the subvolume of the storage pool:
		// mv ${LXD_DIR}/containers/<container_name> ${LXD_DIR}/storage-pools/<pool>/<container_name>
		oldContainerMntPoint := shared.VarPath("containers", ct)
		newContainerMntPoint := getContainerMountPoint(defaultPoolName, ct)
		if shared.PathExists(oldContainerMntPoint) && !shared.PathExists(newContainerMntPoint) {
			err = os.Rename(oldContainerMntPoint, newContainerMntPoint)
			if err != nil {
				err := btrfsSubVolumeCreate(newContainerMntPoint)
				if err != nil {
					return err
				}

				output, err := rsyncLocalCopy(oldContainerMntPoint, newContainerMntPoint, "")
				if err != nil {
					logger.Errorf("Failed to rsync: %s: %s.", output, err)
					return err
				}

				btrfsSubVolumesDelete(oldContainerMntPoint)
				if shared.PathExists(oldContainerMntPoint) {
					err = os.RemoveAll(oldContainerMntPoint)
					if err != nil {
						return err
					}
				}
			}
		}

		// Create a symlink to the mountpoint of the container:
		// ${LXD_DIR}/containers/<container_name> ->
		// ${LXD_DIR}/storage-pools/<pool>/containers/<container_name>
		doesntMatter := false
		err = createContainerMountpoint(newContainerMntPoint, oldContainerMntPoint, doesntMatter)
		if err != nil {
			return err
		}

		// Check if we need to account for snapshots for this container.
		ctSnapshots, err := db.ContainerGetSnapshots(d.db, ct)
		if err != nil {
			return err
		}

		if len(ctSnapshots) > 0 {
			// Create the snapshots directory in
			// the new storage pool:
			// ${LXD_DIR}/storage-pools/<pool>/snapshots
			newSnapshotsMntPoint := getSnapshotMountPoint(defaultPoolName, ct)
			if !shared.PathExists(newSnapshotsMntPoint) {
				err := os.MkdirAll(newSnapshotsMntPoint, 0700)
				if err != nil {
					return err
				}
			}
		}

		for _, cs := range ctSnapshots {
			// Insert storage volumes for snapshots into the
			// database. Note that snapshots have already been moved
			// and symlinked above. So no need to do any work here.
			// Initialize empty storage volume configuration for the
			// container.
			snapshotPoolVolumeConfig := map[string]string{}
			err = storageVolumeFillDefault(cs, snapshotPoolVolumeConfig, defaultPool)
			if err != nil {
				return err
			}

			_, err = db.StoragePoolVolumeGetTypeID(d.db, cs, storagePoolVolumeTypeContainer, poolID)
			if err == nil {
				logger.Warnf("Storage volumes database already contains an entry for the snapshot.")
				err := db.StoragePoolVolumeUpdate(d.db, cs, storagePoolVolumeTypeContainer, poolID, "", snapshotPoolVolumeConfig)
				if err != nil {
					return err
				}
			} else if err == db.NoSuchObjectError {
				// Insert storage volumes for containers into the database.
				_, err := db.StoragePoolVolumeCreate(d.db, cs, "", storagePoolVolumeTypeContainer, poolID, snapshotPoolVolumeConfig)
				if err != nil {
					logger.Errorf("Could not insert a storage volume for snapshot \"%s\".", cs)
					return err
				}
			} else {
				logger.Errorf("Failed to query database: %s", err)
				return err
			}

			// We need to create a new snapshot since we can't move
			// readonly snapshots.
			oldSnapshotMntPoint := shared.VarPath("snapshots", cs)
			newSnapshotMntPoint := getSnapshotMountPoint(defaultPoolName, cs)
			if shared.PathExists(oldSnapshotMntPoint) && !shared.PathExists(newSnapshotMntPoint) {
				err = btrfsSnapshot(oldSnapshotMntPoint, newSnapshotMntPoint, true)
				if err != nil {
					err := btrfsSubVolumeCreate(newSnapshotMntPoint)
					if err != nil {
						return err
					}

					output, err := rsyncLocalCopy(oldSnapshotMntPoint, newSnapshotMntPoint, "")
					if err != nil {
						logger.Errorf("Failed to rsync: %s: %s.", output, err)
						return err
					}

					btrfsSubVolumesDelete(oldSnapshotMntPoint)
					if shared.PathExists(oldSnapshotMntPoint) {
						err = os.RemoveAll(oldSnapshotMntPoint)
						if err != nil {
							return err
						}
					}
				} else {
					// Delete the old subvolume.
					err = btrfsSubVolumesDelete(oldSnapshotMntPoint)
					if err != nil {
						return err
					}
				}
			}
		}

		if len(ctSnapshots) > 0 {
			// Create a new symlink from the snapshots directory of
			// the container to the snapshots directory on the
			// storage pool:
			// ${LXD_DIR}/snapshots/<container_name> -> ${LXD_DIR}/storage-pools/<pool>/snapshots/<container_name>
			snapshotsPath := shared.VarPath("snapshots", ct)
			newSnapshotMntPoint := getSnapshotMountPoint(defaultPoolName, ct)
			os.Remove(snapshotsPath)
			if !shared.PathExists(snapshotsPath) {
				err := os.Symlink(newSnapshotMntPoint, snapshotsPath)
				if err != nil {
					return err
				}
			}
		}
	}

	// Insert storage volumes for images into the database. Images don't
	// move. The tarballs remain in their original location.
	images := append(imgPublic, imgPrivate...)
	for _, img := range images {
		imagePoolVolumeConfig := map[string]string{}
		err = storageVolumeFillDefault(img, imagePoolVolumeConfig, defaultPool)
		if err != nil {
			return err
		}

		_, err = db.StoragePoolVolumeGetTypeID(d.db, img, storagePoolVolumeTypeImage, poolID)
		if err == nil {
			logger.Warnf("Storage volumes database already contains an entry for the image.")
			err := db.StoragePoolVolumeUpdate(d.db, img, storagePoolVolumeTypeImage, poolID, "", imagePoolVolumeConfig)
			if err != nil {
				return err
			}
		} else if err == db.NoSuchObjectError {
			// Insert storage volumes for containers into the database.
			_, err := db.StoragePoolVolumeCreate(d.db, img, "", storagePoolVolumeTypeImage, poolID, imagePoolVolumeConfig)
			if err != nil {
				logger.Errorf("Could not insert a storage volume for image \"%s\".", img)
				return err
			}
		} else {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		imagesMntPoint := getImageMountPoint(defaultPoolName, "")
		if !shared.PathExists(imagesMntPoint) {
			err := os.MkdirAll(imagesMntPoint, 0700)
			if err != nil {
				return err
			}
		}

		oldImageMntPoint := shared.VarPath("images", img+".btrfs")
		newImageMntPoint := getImageMountPoint(defaultPoolName, img)
		if shared.PathExists(oldImageMntPoint) && !shared.PathExists(newImageMntPoint) {
			err := os.Rename(oldImageMntPoint, newImageMntPoint)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func upgradeFromStorageTypeDir(name string, d *Daemon, defaultPoolName string, defaultStorageTypeName string, cRegular []string, cSnapshots []string, imgPublic []string, imgPrivate []string) error {
	poolConfig := map[string]string{}
	poolConfig["source"] = shared.VarPath("storage-pools", defaultPoolName)

	err := storagePoolValidateConfig(defaultPoolName, defaultStorageTypeName, poolConfig, nil)
	if err != nil {
		return err
	}

	err = storagePoolFillDefault(defaultPoolName, defaultStorageTypeName, poolConfig)
	if err != nil {
		return err
	}

	poolID := int64(-1)
	pools, err := db.StoragePools(d.db)
	if err == nil { // Already exist valid storage pools.
		// Check if the storage pool already has a db entry.
		if shared.StringInSlice(defaultPoolName, pools) {
			logger.Warnf("Database already contains a valid entry for the storage pool: %s.", defaultPoolName)
		}

		// Get the pool ID as we need it for storage volume creation.
		// (Use a tmp variable as Go's scoping is freaking me out.)
		tmp, pool, err := db.StoragePoolGet(d.db, defaultPoolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s.", err)
			return err
		}
		poolID = tmp

		// Update the pool configuration on a post LXD 2.9.1 instance
		// that still runs this upgrade code because of a partial
		// upgrade.
		if pool.Config == nil {
			pool.Config = poolConfig
		}
		err = db.StoragePoolUpdate(d.db, defaultPoolName, pool.Description, pool.Config)
		if err != nil {
			return err
		}
	} else if err == db.NoSuchObjectError { // Likely a pristine upgrade.
		tmp, err := dbStoragePoolCreateAndUpdateCache(d.db, defaultPoolName, "", defaultStorageTypeName, poolConfig)
		if err != nil {
			return err
		}
		poolID = tmp

		s, err := storagePoolInit(d.State(), defaultPoolName)
		if err != nil {
			return err
		}

		err = s.StoragePoolCreate()
		if err != nil {
			return err
		}
	} else { // Shouldn't happen.
		logger.Errorf("Failed to query database: %s.", err)
		return err
	}

	// Get storage pool from the db after having updated it above.
	_, defaultPool, err := db.StoragePoolGet(d.db, defaultPoolName)
	if err != nil {
		return err
	}

	// Insert storage volumes for containers into the database.
	for _, ct := range cRegular {
		// Initialize empty storage volume configuration for the
		// container.
		containerPoolVolumeConfig := map[string]string{}
		err = storageVolumeFillDefault(ct, containerPoolVolumeConfig, defaultPool)
		if err != nil {
			return err
		}

		_, err = db.StoragePoolVolumeGetTypeID(d.db, ct, storagePoolVolumeTypeContainer, poolID)
		if err == nil {
			logger.Warnf("Storage volumes database already contains an entry for the container.")
			err := db.StoragePoolVolumeUpdate(d.db, ct, storagePoolVolumeTypeContainer, poolID, "", containerPoolVolumeConfig)
			if err != nil {
				return err
			}
		} else if err == db.NoSuchObjectError {
			// Insert storage volumes for containers into the database.
			_, err := db.StoragePoolVolumeCreate(d.db, ct, "", storagePoolVolumeTypeContainer, poolID, containerPoolVolumeConfig)
			if err != nil {
				logger.Errorf("Could not insert a storage volume for container \"%s\".", ct)
				return err
			}
		} else {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// Create the new path where containers will be located on the
		// new storage api.
		containersMntPoint := getContainerMountPoint(defaultPoolName, "")
		if !shared.PathExists(containersMntPoint) {
			err := os.MkdirAll(containersMntPoint, 0711)
			if err != nil {
				return err
			}
		}

		// Simply rename the container when they are directories.
		oldContainerMntPoint := shared.VarPath("containers", ct)
		newContainerMntPoint := getContainerMountPoint(defaultPoolName, ct)
		if shared.PathExists(oldContainerMntPoint) && !shared.PathExists(newContainerMntPoint) {
			// First try to rename.
			err := os.Rename(oldContainerMntPoint, newContainerMntPoint)
			if err != nil {
				output, err := rsyncLocalCopy(oldContainerMntPoint, newContainerMntPoint, "")
				if err != nil {
					logger.Errorf("Failed to rsync: %s: %s.", output, err)
					return err
				}
				err = os.RemoveAll(oldContainerMntPoint)
				if err != nil {
					return err
				}
			}
		}

		doesntMatter := false
		err = createContainerMountpoint(newContainerMntPoint, oldContainerMntPoint, doesntMatter)
		if err != nil {
			return err
		}

		// Check if we need to account for snapshots for this container.
		oldSnapshotMntPoint := shared.VarPath("snapshots", ct)
		if !shared.PathExists(oldSnapshotMntPoint) {
			continue
		}

		// If the snapshots directory for that container is empty,
		// remove it.
		isEmpty, err := shared.PathIsEmpty(oldSnapshotMntPoint)
		if isEmpty {
			os.Remove(oldSnapshotMntPoint)
			continue
		}

		// Create the new path where snapshots will be located on the
		// new storage api.
		snapshotsMntPoint := shared.VarPath("storage-pools", defaultPoolName, "snapshots")
		if !shared.PathExists(snapshotsMntPoint) {
			err := os.MkdirAll(snapshotsMntPoint, 0711)
			if err != nil {
				return err
			}
		}

		// Now simply rename the snapshots directory as well.
		newSnapshotMntPoint := getSnapshotMountPoint(defaultPoolName, ct)
		if shared.PathExists(oldSnapshotMntPoint) && !shared.PathExists(newSnapshotMntPoint) {
			err := os.Rename(oldSnapshotMntPoint, newSnapshotMntPoint)
			if err != nil {
				output, err := rsyncLocalCopy(oldSnapshotMntPoint, newSnapshotMntPoint, "")
				if err != nil {
					logger.Errorf("Failed to rsync: %s: %s.", output, err)
					return err
				}
				err = os.RemoveAll(oldSnapshotMntPoint)
				if err != nil {
					return err
				}
			}
		}

		// Create a symlink for this container.  snapshots.
		err = createSnapshotMountpoint(newSnapshotMntPoint, newSnapshotMntPoint, oldSnapshotMntPoint)
		if err != nil {
			return err
		}
	}

	// Insert storage volumes for snapshots into the database. Note that
	// snapshots have already been moved and symlinked above. So no need to
	// do any work here.
	for _, cs := range cSnapshots {
		// Insert storage volumes for snapshots into the
		// database. Note that snapshots have already been moved
		// and symlinked above. So no need to do any work here.
		// Initialize empty storage volume configuration for the
		// container.
		snapshotPoolVolumeConfig := map[string]string{}
		err = storageVolumeFillDefault(cs, snapshotPoolVolumeConfig, defaultPool)
		if err != nil {
			return err
		}

		_, err = db.StoragePoolVolumeGetTypeID(d.db, cs, storagePoolVolumeTypeContainer, poolID)
		if err == nil {
			logger.Warnf("Storage volumes database already contains an entry for the snapshot.")
			err := db.StoragePoolVolumeUpdate(d.db, cs, storagePoolVolumeTypeContainer, poolID, "", snapshotPoolVolumeConfig)
			if err != nil {
				return err
			}
		} else if err == db.NoSuchObjectError {
			// Insert storage volumes for containers into the database.
			_, err := db.StoragePoolVolumeCreate(d.db, cs, "", storagePoolVolumeTypeContainer, poolID, snapshotPoolVolumeConfig)
			if err != nil {
				logger.Errorf("Could not insert a storage volume for snapshot \"%s\".", cs)
				return err
			}
		} else {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}
	}

	// Insert storage volumes for images into the database. Images don't
	// move. The tarballs remain in their original location.
	images := append(imgPublic, imgPrivate...)
	for _, img := range images {
		imagePoolVolumeConfig := map[string]string{}
		err = storageVolumeFillDefault(img, imagePoolVolumeConfig, defaultPool)
		if err != nil {
			return err
		}

		_, err = db.StoragePoolVolumeGetTypeID(d.db, img, storagePoolVolumeTypeImage, poolID)
		if err == nil {
			logger.Warnf("Storage volumes database already contains an entry for the image.")
			err := db.StoragePoolVolumeUpdate(d.db, img, storagePoolVolumeTypeImage, poolID, "", imagePoolVolumeConfig)
			if err != nil {
				return err
			}
		} else if err == db.NoSuchObjectError {
			// Insert storage volumes for containers into the database.
			_, err := db.StoragePoolVolumeCreate(d.db, img, "", storagePoolVolumeTypeImage, poolID, imagePoolVolumeConfig)
			if err != nil {
				logger.Errorf("Could not insert a storage volume for image \"%s\".", img)
				return err
			}
		} else {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}
	}

	return nil
}

func upgradeFromStorageTypeLvm(name string, d *Daemon, defaultPoolName string, defaultStorageTypeName string, cRegular []string, cSnapshots []string, imgPublic []string, imgPrivate []string) error {
	poolConfig := map[string]string{}
	poolConfig["source"] = defaultPoolName

	// Set it only if it is not the default value.
	fsType := daemonConfig["storage.lvm_fstype"].Get()
	if fsType != "" && fsType != "ext4" {
		poolConfig["volume.block.filesystem"] = fsType
	}

	// Set it only if it is not the default value.
	fsMntOpts := daemonConfig["storage.lvm_mount_options"].Get()
	if fsMntOpts != "" && fsMntOpts != "discard" {
		poolConfig["volume.block.mount_options"] = fsMntOpts
	}

	thinPoolName := "LXDPool"
	poolConfig["lvm.thinpool_name"] = daemonConfig["storage.lvm_thinpool_name"].Get()
	if poolConfig["lvm.thinpool_name"] != "" {
		thinPoolName = poolConfig["lvm.thinpool_name"]
	} else {
		// If empty we need to set it to the old default.
		poolConfig["lvm.thinpool_name"] = thinPoolName
	}

	poolConfig["lvm.vg_name"] = daemonConfig["storage.lvm_vg_name"].Get()

	poolConfig["volume.size"] = daemonConfig["storage.lvm_volume_size"].Get()
	if poolConfig["volume.size"] != "" {
		// In case stuff like GiB is used which
		// share.dParseByteSizeString() doesn't handle.
		if strings.Contains(poolConfig["volume.size"], "i") {
			poolConfig["volume.size"] = strings.Replace(poolConfig["volume.size"], "i", "", 1)
		}
	}
	// On previous upgrade versions, "size" was set instead of
	// "volume.size", so unset it.
	poolConfig["size"] = ""

	err := storagePoolValidateConfig(defaultPoolName, defaultStorageTypeName, poolConfig, nil)
	if err != nil {
		return err
	}

	err = storagePoolFillDefault(defaultPoolName, defaultStorageTypeName, poolConfig)
	if err != nil {
		return err
	}

	// Activate volume group
	err = storageVGActivate(defaultPoolName)
	if err != nil {
		logger.Errorf("Could not activate volume group \"%s\". Manual intervention needed.", defaultPoolName)
		return err
	}

	// Peek into the storage pool database to see whether any storage pools
	// are already configured. If so, we can assume that a partial upgrade
	// has been performed and can skip the next steps.
	poolID := int64(-1)
	pools, err := db.StoragePools(d.db)
	if err == nil { // Already exist valid storage pools.
		// Check if the storage pool already has a db entry.
		if shared.StringInSlice(defaultPoolName, pools) {
			logger.Warnf("Database already contains a valid entry for the storage pool: %s.", defaultPoolName)
		}

		// Get the pool ID as we need it for storage volume creation.
		// (Use a tmp variable as Go's scoping is freaking me out.)
		tmp, pool, err := db.StoragePoolGet(d.db, defaultPoolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s.", err)
			return err
		}
		poolID = tmp

		// Update the pool configuration on a post LXD 2.9.1 instance
		// that still runs this upgrade code because of a partial
		// upgrade.
		if pool.Config == nil {
			pool.Config = poolConfig
		}
		err = db.StoragePoolUpdate(d.db, defaultPoolName, pool.Description, pool.Config)
		if err != nil {
			return err
		}
	} else if err == db.NoSuchObjectError { // Likely a pristine upgrade.
		tmp, err := dbStoragePoolCreateAndUpdateCache(d.db, defaultPoolName, "", defaultStorageTypeName, poolConfig)
		if err != nil {
			return err
		}
		poolID = tmp
	} else { // Shouldn't happen.
		logger.Errorf("Failed to query database: %s.", err)
		return err
	}

	// Create pool mountpoint if it doesn't already exist.
	poolMntPoint := getStoragePoolMountPoint(defaultPoolName)
	if !shared.PathExists(poolMntPoint) {
		err = os.MkdirAll(poolMntPoint, 0711)
		if err != nil {
			logger.Warnf("Failed to create pool mountpoint: %s", poolMntPoint)
		}
	}

	if len(cRegular) > 0 {
		// Create generic containers folder on the storage pool.
		newContainersMntPoint := getContainerMountPoint(defaultPoolName, "")
		if !shared.PathExists(newContainersMntPoint) {
			err = os.MkdirAll(newContainersMntPoint, 0711)
			if err != nil {
				logger.Warnf("Failed to create containers mountpoint: %s", newContainersMntPoint)
			}
		}
	}

	// Get storage pool from the db after having updated it above.
	_, defaultPool, err := db.StoragePoolGet(d.db, defaultPoolName)
	if err != nil {
		return err
	}

	// Insert storage volumes for containers into the database.
	for _, ct := range cRegular {
		// Initialize empty storage volume configuration for the
		// container.
		containerPoolVolumeConfig := map[string]string{}
		err = storageVolumeFillDefault(ct, containerPoolVolumeConfig, defaultPool)
		if err != nil {
			return err
		}

		_, err = db.StoragePoolVolumeGetTypeID(d.db, ct, storagePoolVolumeTypeContainer, poolID)
		if err == nil {
			logger.Warnf("Storage volumes database already contains an entry for the container.")
			err := db.StoragePoolVolumeUpdate(d.db, ct, storagePoolVolumeTypeContainer, poolID, "", containerPoolVolumeConfig)
			if err != nil {
				return err
			}
		} else if err == db.NoSuchObjectError {
			// Insert storage volumes for containers into the database.
			_, err := db.StoragePoolVolumeCreate(d.db, ct, "", storagePoolVolumeTypeContainer, poolID, containerPoolVolumeConfig)
			if err != nil {
				logger.Errorf("Could not insert a storage volume for container \"%s\".", ct)
				return err
			}
		} else {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// Unmount the logical volume.
		oldContainerMntPoint := shared.VarPath("containers", ct)
		if shared.IsMountPoint(oldContainerMntPoint) {
			err := tryUnmount(oldContainerMntPoint, syscall.MNT_DETACH)
			if err != nil {
				logger.Errorf("Failed to unmount LVM logical volume \"%s\": %s.", oldContainerMntPoint, err)
				return err
			}
		}

		// Create the new path where containers will be located on the
		// new storage api. We do os.Rename() here to preserve
		// permissions and ownership.
		newContainerMntPoint := getContainerMountPoint(defaultPoolName, ct)
		ctLvName := containerNameToLVName(ct)
		newContainerLvName := fmt.Sprintf("%s_%s", storagePoolVolumeAPIEndpointContainers, ctLvName)
		containerLvDevPath := getLvmDevPath(defaultPoolName, storagePoolVolumeAPIEndpointContainers, ctLvName)
		if !shared.PathExists(containerLvDevPath) {
			oldLvDevPath := fmt.Sprintf("/dev/%s/%s", defaultPoolName, ctLvName)
			// If the old LVM device path for the logical volume
			// exists we call lvrename. Otherwise this is likely a
			// mixed-storage LXD instance which we need to deal
			// with.
			if shared.PathExists(oldLvDevPath) {
				// Rename the logical volume mountpoint.
				if shared.PathExists(oldContainerMntPoint) && !shared.PathExists(newContainerMntPoint) {
					err = os.Rename(oldContainerMntPoint, newContainerMntPoint)
					if err != nil {
						logger.Errorf("Failed to rename LVM container mountpoint from %s to %s: %s.", oldContainerMntPoint, newContainerMntPoint, err)
						return err
					}
				}

				// Remove the old container mountpoint.
				if shared.PathExists(oldContainerMntPoint + ".lv") {
					err := os.Remove(oldContainerMntPoint + ".lv")
					if err != nil {
						logger.Errorf("Failed to remove old LVM container mountpoint %s: %s.", oldContainerMntPoint+".lv", err)
						return err
					}
				}

				// Rename the logical volume.
				msg, err := shared.TryRunCommand("lvrename", defaultPoolName, ctLvName, newContainerLvName)
				if err != nil {
					logger.Errorf("Failed to rename LVM logical volume from %s to %s: %s.", ctLvName, newContainerLvName, msg)
					return err
				}
			} else if shared.PathExists(oldContainerMntPoint) && shared.IsDir(oldContainerMntPoint) {
				// This is a directory backed container and it
				// means that this was a mixed-storage LXD
				// instance.

				// Initialize storage interface for the new
				// container.
				ctStorage, err := storagePoolVolumeContainerLoadInit(d.State(), ct)
				if err != nil {
					logger.Errorf("Failed to initialize new storage interface for LVM container %s: %s.", ct, err)
					return err
				}

				// Load the container from the database.
				ctStruct, err := containerLoadByName(d.State(), ct)
				if err != nil {
					logger.Errorf("Failed to load LVM container %s: %s.", ct, err)
					return err
				}

				// Create an empty LVM logical volume for the
				// container.
				err = ctStorage.ContainerCreate(ctStruct)
				if err != nil {
					logger.Errorf("Failed to create empty LVM logical volume for container %s: %s.", ct, err)
					return err
				}

				// In case the new LVM logical volume for the
				// container is not mounted mount it.
				if !shared.IsMountPoint(newContainerMntPoint) {
					_, err = ctStorage.ContainerMount(ctStruct)
					if err != nil {
						logger.Errorf("Failed to mount new empty LVM logical volume for container %s: %s.", ct, err)
						return err
					}
				}

				// Use rsync to fill the empty volume.
				output, err := rsyncLocalCopy(oldContainerMntPoint, newContainerMntPoint, "")
				if err != nil {
					ctStorage.ContainerDelete(ctStruct)
					return fmt.Errorf("rsync failed: %s", string(output))
				}

				// Remove the old container.
				err = os.RemoveAll(oldContainerMntPoint)
				if err != nil {
					logger.Errorf("Failed to remove old container %s: %s.", oldContainerMntPoint, err)
					return err
				}
			}
		}

		// Create the new container mountpoint.
		doesntMatter := false
		err = createContainerMountpoint(newContainerMntPoint, oldContainerMntPoint, doesntMatter)
		if err != nil {
			logger.Errorf("Failed to create container mountpoint \"%s\" for LVM logical volume: %s.", newContainerMntPoint, err)
			return err
		}

		// Guaranteed to be set.
		lvFsType := containerPoolVolumeConfig["block.filesystem"]
		mountOptions := containerPoolVolumeConfig["block.mount_options"]
		if mountOptions == "" {
			// Set to default.
			mountOptions = "discard"
		}

		// Check if we need to account for snapshots for this container.
		ctSnapshots, err := db.ContainerGetSnapshots(d.db, ct)
		if err != nil {
			return err
		}

		for _, cs := range ctSnapshots {
			// Insert storage volumes for snapshots into the
			// database. Note that snapshots have already been moved
			// and symlinked above. So no need to do any work here.
			// Initialize empty storage volume configuration for the
			// container.
			snapshotPoolVolumeConfig := map[string]string{}
			err = storageVolumeFillDefault(cs, snapshotPoolVolumeConfig, defaultPool)
			if err != nil {
				return err
			}

			_, err = db.StoragePoolVolumeGetTypeID(d.db, cs, storagePoolVolumeTypeContainer, poolID)
			if err == nil {
				logger.Warnf("Storage volumes database already contains an entry for the snapshot.")
				err := db.StoragePoolVolumeUpdate(d.db, cs, storagePoolVolumeTypeContainer, poolID, "", snapshotPoolVolumeConfig)
				if err != nil {
					return err
				}
			} else if err == db.NoSuchObjectError {
				// Insert storage volumes for containers into the database.
				_, err := db.StoragePoolVolumeCreate(d.db, cs, "", storagePoolVolumeTypeContainer, poolID, snapshotPoolVolumeConfig)
				if err != nil {
					logger.Errorf("Could not insert a storage volume for snapshot \"%s\".", cs)
					return err
				}
			} else {
				logger.Errorf("Failed to query database: %s", err)
				return err
			}

			// Create the snapshots directory in the new storage
			// pool:
			// ${LXD_DIR}/storage-pools/<pool>/snapshots
			newSnapshotMntPoint := getSnapshotMountPoint(defaultPoolName, cs)
			if !shared.PathExists(newSnapshotMntPoint) {
				err := os.MkdirAll(newSnapshotMntPoint, 0700)
				if err != nil {
					return err
				}
			}

			oldSnapshotMntPoint := shared.VarPath("snapshots", cs)
			os.Remove(oldSnapshotMntPoint + ".lv")

			// Make sure we use a valid lv name.
			csLvName := containerNameToLVName(cs)
			newSnapshotLvName := fmt.Sprintf("%s_%s", storagePoolVolumeAPIEndpointContainers, csLvName)
			snapshotLvDevPath := getLvmDevPath(defaultPoolName, storagePoolVolumeAPIEndpointContainers, csLvName)
			if !shared.PathExists(snapshotLvDevPath) {
				oldLvDevPath := fmt.Sprintf("/dev/%s/%s", defaultPoolName, csLvName)
				if shared.PathExists(oldLvDevPath) {
					// Unmount the logical volume.
					if shared.IsMountPoint(oldSnapshotMntPoint) {
						err := tryUnmount(oldSnapshotMntPoint, syscall.MNT_DETACH)
						if err != nil {
							logger.Errorf("Failed to unmount LVM logical volume \"%s\": %s.", oldSnapshotMntPoint, err)
							return err
						}
					}

					// Rename the snapshot mountpoint to preserve acl's and
					// so on.
					if shared.PathExists(oldSnapshotMntPoint) && !shared.PathExists(newSnapshotMntPoint) {
						err := os.Rename(oldSnapshotMntPoint, newSnapshotMntPoint)
						if err != nil {
							logger.Errorf("Failed to rename LVM container mountpoint from %s to %s: %s.", oldSnapshotMntPoint, newSnapshotMntPoint, err)
							return err
						}
					}

					// Rename the logical volume.
					msg, err := shared.TryRunCommand("lvrename", defaultPoolName, csLvName, newSnapshotLvName)
					if err != nil {
						logger.Errorf("Failed to rename LVM logical volume from %s to %s: %s.", csLvName, newSnapshotLvName, msg)
						return err
					}
				} else if shared.PathExists(oldSnapshotMntPoint) && shared.IsDir(oldSnapshotMntPoint) {
					// This is a directory backed container
					// and it means that this was a
					// mixed-storage LXD instance.

					// Initialize storage interface for the new
					// snapshot.
					csStorage, err := storagePoolVolumeContainerLoadInit(d.State(), cs)
					if err != nil {
						logger.Errorf("Failed to initialize new storage interface for LVM container %s: %s.", cs, err)
						return err
					}

					// Load the snapshot from the database.
					csStruct, err := containerLoadByName(d.State(), cs)
					if err != nil {
						logger.Errorf("Failed to load LVM container %s: %s.", cs, err)
						return err
					}

					// Create an empty LVM logical volume
					// for the snapshot.
					err = csStorage.ContainerSnapshotCreateEmpty(csStruct)
					if err != nil {
						logger.Errorf("Failed to create empty LVM logical volume for container %s: %s.", cs, err)
						return err
					}

					// In case the new LVM logical volume
					// for the snapshot is not mounted mount
					// it.
					if !shared.IsMountPoint(newSnapshotMntPoint) {
						_, err = csStorage.ContainerMount(csStruct)
						if err != nil {
							logger.Errorf("Failed to mount new empty LVM logical volume for container %s: %s.", cs, err)
							return err
						}
					}

					// Use rsync to fill the empty volume.
					output, err := rsyncLocalCopy(oldSnapshotMntPoint, newSnapshotMntPoint, "")
					if err != nil {
						csStorage.ContainerDelete(csStruct)
						return fmt.Errorf("rsync failed: %s", string(output))
					}

					// Remove the old snapshot.
					err = os.RemoveAll(oldSnapshotMntPoint)
					if err != nil {
						logger.Errorf("Failed to remove old container %s: %s.", oldSnapshotMntPoint, err)
						return err
					}
				}
			}
		}

		if len(ctSnapshots) > 0 {
			// Create a new symlink from the snapshots directory of
			// the container to the snapshots directory on the
			// storage pool:
			// ${LXD_DIR}/snapshots/<container_name> -> ${LXD_DIR}/storage-pools/<pool>/snapshots/<container_name>
			snapshotsPath := shared.VarPath("snapshots", ct)
			newSnapshotsPath := getSnapshotMountPoint(defaultPoolName, ct)
			if shared.PathExists(snapshotsPath) {
				// On a broken update snapshotsPath will contain
				// empty directories that need to be removed.
				err := os.RemoveAll(snapshotsPath)
				if err != nil {
					return err
				}
			}
			if !shared.PathExists(snapshotsPath) {
				err = os.Symlink(newSnapshotsPath, snapshotsPath)
				if err != nil {
					return err
				}
			}
		}

		if !shared.IsMountPoint(newContainerMntPoint) {
			err := tryMount(containerLvDevPath, newContainerMntPoint, lvFsType, 0, mountOptions)
			if err != nil {
				logger.Errorf("Failed to mount LVM logical \"%s\" onto \"%s\" : %s.", containerLvDevPath, newContainerMntPoint, err)
				return err
			}
		}
	}

	images := append(imgPublic, imgPrivate...)
	if len(images) > 0 {
		imagesMntPoint := getImageMountPoint(defaultPoolName, "")
		if !shared.PathExists(imagesMntPoint) {
			err := os.MkdirAll(imagesMntPoint, 0700)
			if err != nil {
				return err
			}
		}
	}

	for _, img := range images {
		imagePoolVolumeConfig := map[string]string{}
		err = storageVolumeFillDefault(img, imagePoolVolumeConfig, defaultPool)
		if err != nil {
			return err
		}

		_, err = db.StoragePoolVolumeGetTypeID(d.db, img, storagePoolVolumeTypeImage, poolID)
		if err == nil {
			logger.Warnf("Storage volumes database already contains an entry for the image.")
			err := db.StoragePoolVolumeUpdate(d.db, img, storagePoolVolumeTypeImage, poolID, "", imagePoolVolumeConfig)
			if err != nil {
				return err
			}
		} else if err == db.NoSuchObjectError {
			// Insert storage volumes for containers into the database.
			_, err := db.StoragePoolVolumeCreate(d.db, img, "", storagePoolVolumeTypeImage, poolID, imagePoolVolumeConfig)
			if err != nil {
				logger.Errorf("Could not insert a storage volume for image \"%s\".", img)
				return err
			}
		} else {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// Unmount the logical volume.
		oldImageMntPoint := shared.VarPath("images", img+".lv")
		if shared.IsMountPoint(oldImageMntPoint) {
			err := tryUnmount(oldImageMntPoint, syscall.MNT_DETACH)
			if err != nil {
				return err
			}
		}

		if shared.PathExists(oldImageMntPoint) {
			err := os.Remove(oldImageMntPoint)
			if err != nil {
				return err
			}
		}

		newImageMntPoint := getImageMountPoint(defaultPoolName, img)
		if !shared.PathExists(newImageMntPoint) {
			err := os.MkdirAll(newImageMntPoint, 0700)
			if err != nil {
				return err
			}
		}

		// Rename the logical volume device.
		newImageLvName := fmt.Sprintf("%s_%s", storagePoolVolumeAPIEndpointImages, img)
		imageLvDevPath := getLvmDevPath(defaultPoolName, storagePoolVolumeAPIEndpointImages, img)
		oldLvDevPath := fmt.Sprintf("/dev/%s/%s", defaultPoolName, img)
		// Only create logical volumes for images that have a logical
		// volume on the pre-storage-api LXD instance. If not, we don't
		// care since LXD will create a logical volume on demand.
		if !shared.PathExists(imageLvDevPath) && shared.PathExists(oldLvDevPath) {
			_, err := shared.TryRunCommand("lvrename", defaultPoolName, img, newImageLvName)
			if err != nil {
				return err
			}
		}

		if !shared.PathExists(imageLvDevPath) {
			// This image didn't exist as a logical volume on the
			// old LXD instance so we need to kick it from the
			// storage volumes database for this pool.
			err := db.StoragePoolVolumeDelete(d.db, img, storagePoolVolumeTypeImage, poolID)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func upgradeFromStorageTypeZfs(name string, d *Daemon, defaultPoolName string, defaultStorageTypeName string, cRegular []string, cSnapshots []string, imgPublic []string, imgPrivate []string) error {
	poolConfig := map[string]string{}
	oldLoopFilePath := shared.VarPath("zfs.img")
	poolName := defaultPoolName
	// In case we are given a dataset, we need to chose a sensible name.
	if strings.Contains(defaultPoolName, "/") {
		// We are given a dataset and need to chose a sensible name.
		poolName = "default"
	}

	// Peek into the storage pool database to see whether any storage pools
	// are already configured. If so, we can assume that a partial upgrade
	// has been performed and can skip the next steps. Otherwise we might
	// run into problems. For example, the "zfs.img" file might have already
	// been moved into ${LXD_DIR}/disks and we might therefore falsely
	// conclude that we're using an existing storage pool.
	err := storagePoolValidateConfig(poolName, defaultStorageTypeName, poolConfig, nil)
	if err != nil {
		return err
	}

	err = storagePoolFillDefault(poolName, defaultStorageTypeName, poolConfig)
	if err != nil {
		return err
	}

	// Peek into the storage pool database to see whether any storage pools
	// are already configured. If so, we can assume that a partial upgrade
	// has been performed and can skip the next steps.
	poolID := int64(-1)
	pools, err := db.StoragePools(d.db)
	if err == nil { // Already exist valid storage pools.
		// Check if the storage pool already has a db entry.
		if shared.StringInSlice(poolName, pools) {
			logger.Warnf("Database already contains a valid entry for the storage pool: %s.", poolName)
		}

		// Get the pool ID as we need it for storage volume creation.
		// (Use a tmp variable as Go's scoping is freaking me out.)
		tmp, pool, err := db.StoragePoolGet(d.db, poolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s.", err)
			return err
		}
		poolID = tmp

		// Update the pool configuration on a post LXD 2.9.1 instance
		// that still runs this upgrade code because of a partial
		// upgrade.
		if pool.Config == nil {
			pool.Config = poolConfig
		}
		err = db.StoragePoolUpdate(d.db, poolName, pool.Description, pool.Config)
		if err != nil {
			return err
		}
	} else if err == db.NoSuchObjectError { // Likely a pristine upgrade.
		if shared.PathExists(oldLoopFilePath) {
			// This is a loop file pool.
			poolConfig["source"] = shared.VarPath("disks", poolName+".img")
			err := shared.FileMove(oldLoopFilePath, poolConfig["source"])
			if err != nil {
				return err
			}
		} else {
			// This is a block device pool.
			// Here, we need to use "defaultPoolName" since we want
			// to refer to the on-disk name of the pool in the
			// "source" propert and not the db name of the pool.
			poolConfig["source"] = defaultPoolName
		}

		// Querying the size of a storage pool only makes sense when it
		// is not a dataset.
		if poolName == defaultPoolName {
			output, err := shared.RunCommand("zpool", "get", "size", "-p", "-H", defaultPoolName)
			if err == nil {
				lidx := strings.LastIndex(output, "\t")
				fidx := strings.LastIndex(output[:lidx-1], "\t")
				poolConfig["size"] = output[fidx+1 : lidx]
			}
		}

		// (Use a tmp variable as Go's scoping is freaking me out.)
		tmp, err := dbStoragePoolCreateAndUpdateCache(d.db, poolName, "", defaultStorageTypeName, poolConfig)
		if err != nil {
			logger.Warnf("Storage pool already exists in the database. Proceeding...")
		}
		poolID = tmp
	} else { // Shouldn't happen.
		logger.Errorf("Failed to query database: %s.", err)
		return err
	}

	// Get storage pool from the db after having updated it above.
	_, defaultPool, err := db.StoragePoolGet(d.db, poolName)
	if err != nil {
		return err
	}

	if len(cRegular) > 0 {
		containersSubvolumePath := getContainerMountPoint(poolName, "")
		if !shared.PathExists(containersSubvolumePath) {
			err := os.MkdirAll(containersSubvolumePath, 0711)
			if err != nil {
				logger.Warnf("Failed to create path: %s.", containersSubvolumePath)
			}
		}
	}

	failedUpgradeEntities := []string{}
	for _, ct := range cRegular {
		// Initialize empty storage volume configuration for the
		// container.
		containerPoolVolumeConfig := map[string]string{}
		err = storageVolumeFillDefault(ct, containerPoolVolumeConfig, defaultPool)
		if err != nil {
			return err
		}

		_, err = db.StoragePoolVolumeGetTypeID(d.db, ct, storagePoolVolumeTypeContainer, poolID)
		if err == nil {
			logger.Warnf("Storage volumes database already contains an entry for the container.")
			err := db.StoragePoolVolumeUpdate(d.db, ct, storagePoolVolumeTypeContainer, poolID, "", containerPoolVolumeConfig)
			if err != nil {
				return err
			}
		} else if err == db.NoSuchObjectError {
			// Insert storage volumes for containers into the database.
			_, err := db.StoragePoolVolumeCreate(d.db, ct, "", storagePoolVolumeTypeContainer, poolID, containerPoolVolumeConfig)
			if err != nil {
				logger.Errorf("Could not insert a storage volume for container \"%s\".", ct)
				return err
			}
		} else {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// Unmount the container zfs doesn't really seem to care if we
		// do this.
		// Here "defaultPoolName" must be used since we want to refer to
		// the on-disk name of the zfs pool when moving the datasets
		// around.
		ctDataset := fmt.Sprintf("%s/containers/%s", defaultPoolName, ct)
		oldContainerMntPoint := shared.VarPath("containers", ct)
		if shared.IsMountPoint(oldContainerMntPoint) {
			_, err := shared.TryRunCommand("zfs", "unmount", "-f", ctDataset)
			if err != nil {
				logger.Warnf("Failed to unmount ZFS filesystem via zfs unmount. Trying lazy umount (MNT_DETACH)...")
				err := tryUnmount(oldContainerMntPoint, syscall.MNT_DETACH)
				if err != nil {
					failedUpgradeEntities = append(failedUpgradeEntities, fmt.Sprintf("containers/%s: Failed to umount zfs filesystem.", ct))
					continue
				}
			}
		}

		os.Remove(oldContainerMntPoint)

		os.Remove(oldContainerMntPoint + ".zfs")

		// Changing the mountpoint property should have actually created
		// the path but in case it somehow didn't let's do it ourselves.
		doesntMatter := false
		newContainerMntPoint := getContainerMountPoint(poolName, ct)
		err = createContainerMountpoint(newContainerMntPoint, oldContainerMntPoint, doesntMatter)
		if err != nil {
			logger.Warnf("Failed to create mountpoint for the container: %s.", newContainerMntPoint)
			failedUpgradeEntities = append(failedUpgradeEntities, fmt.Sprintf("containers/%s: Failed to create container mountpoint: %s", ct, err))
			continue
		}

		// Set new mountpoint for the container's dataset it will be
		// automatically mounted.
		output, err := shared.RunCommand(
			"zfs",
			"set",
			fmt.Sprintf("mountpoint=%s", newContainerMntPoint),
			ctDataset)
		if err != nil {
			logger.Warnf("Failed to set new ZFS mountpoint: %s.", output)
			failedUpgradeEntities = append(failedUpgradeEntities, fmt.Sprintf("containers/%s: Failed to set new zfs mountpoint: %s", ct, err))
			continue
		}

		// Check if we need to account for snapshots for this container.
		ctSnapshots, err := db.ContainerGetSnapshots(d.db, ct)
		if err != nil {
			logger.Errorf("Failed to query database")
			return err
		}

		snapshotsPath := shared.VarPath("snapshots", ct)
		for _, cs := range ctSnapshots {
			// Insert storage volumes for snapshots into the
			// database. Note that snapshots have already been moved
			// and symlinked above. So no need to do any work here.
			// Initialize empty storage volume configuration for the
			// container.
			snapshotPoolVolumeConfig := map[string]string{}
			err = storageVolumeFillDefault(cs, snapshotPoolVolumeConfig, defaultPool)
			if err != nil {
				return err
			}

			_, err = db.StoragePoolVolumeGetTypeID(d.db, cs, storagePoolVolumeTypeContainer, poolID)
			if err == nil {
				logger.Warnf("Storage volumes database already contains an entry for the snapshot.")
				err := db.StoragePoolVolumeUpdate(d.db, cs, storagePoolVolumeTypeContainer, poolID, "", snapshotPoolVolumeConfig)
				if err != nil {
					return err
				}
			} else if err == db.NoSuchObjectError {
				// Insert storage volumes for containers into the database.
				_, err := db.StoragePoolVolumeCreate(d.db, cs, "", storagePoolVolumeTypeContainer, poolID, snapshotPoolVolumeConfig)
				if err != nil {
					logger.Errorf("Could not insert a storage volume for snapshot \"%s\".", cs)
					return err
				}
			} else {
				logger.Errorf("Failed to query database: %s", err)
				return err
			}

			// Create the new mountpoint for snapshots in the new
			// storage api.
			newSnapshotMntPoint := getSnapshotMountPoint(poolName, cs)
			if !shared.PathExists(newSnapshotMntPoint) {
				err = os.MkdirAll(newSnapshotMntPoint, 0711)
				if err != nil {
					logger.Warnf("Failed to create mountpoint for snapshot: %s.", newSnapshotMntPoint)
					failedUpgradeEntities = append(failedUpgradeEntities, fmt.Sprintf("snapshots/%s: Failed to create mountpoint for snapshot.", cs))
					continue
				}
			}
		}

		os.RemoveAll(snapshotsPath)

		// Create a symlink for this container's snapshots.
		if len(ctSnapshots) != 0 {
			newSnapshotsMntPoint := getSnapshotMountPoint(poolName, ct)
			if !shared.PathExists(newSnapshotsMntPoint) {
				err := os.Symlink(newSnapshotsMntPoint, snapshotsPath)
				if err != nil {
					logger.Warnf("Failed to create symlink for snapshots: %s -> %s.", snapshotsPath, newSnapshotsMntPoint)
				}
			}
		}
	}

	// Insert storage volumes for images into the database. Images don't
	// move. The tarballs remain in their original location.
	images := append(imgPublic, imgPrivate...)
	for _, img := range images {
		imagePoolVolumeConfig := map[string]string{}
		err = storageVolumeFillDefault(img, imagePoolVolumeConfig, defaultPool)
		if err != nil {
			return err
		}

		_, err = db.StoragePoolVolumeGetTypeID(d.db, img, storagePoolVolumeTypeImage, poolID)
		if err == nil {
			logger.Warnf("Storage volumes database already contains an entry for the image.")
			err := db.StoragePoolVolumeUpdate(d.db, img, storagePoolVolumeTypeImage, poolID, "", imagePoolVolumeConfig)
			if err != nil {
				return err
			}
		} else if err == db.NoSuchObjectError {
			// Insert storage volumes for containers into the database.
			_, err := db.StoragePoolVolumeCreate(d.db, img, "", storagePoolVolumeTypeImage, poolID, imagePoolVolumeConfig)
			if err != nil {
				logger.Errorf("Could not insert a storage volume for image \"%s\".", img)
				return err
			}
		} else {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		imageMntPoint := getImageMountPoint(poolName, img)
		if !shared.PathExists(imageMntPoint) {
			err := os.MkdirAll(imageMntPoint, 0700)
			if err != nil {
				logger.Warnf("Failed to create image mountpoint. Proceeding...")
			}
		}

		oldImageMntPoint := shared.VarPath("images", img+".zfs")
		// Here "defaultPoolName" must be used since we want to refer to
		// the on-disk name of the zfs pool when moving the datasets
		// around.
		imageDataset := fmt.Sprintf("%s/images/%s", defaultPoolName, img)
		if shared.PathExists(oldImageMntPoint) && shared.IsMountPoint(oldImageMntPoint) {
			_, err := shared.TryRunCommand("zfs", "unmount", "-f", imageDataset)
			if err != nil {
				logger.Warnf("Failed to unmount ZFS filesystem via zfs unmount. Trying lazy umount (MNT_DETACH)...")
				err := tryUnmount(oldImageMntPoint, syscall.MNT_DETACH)
				if err != nil {
					logger.Warnf("Failed to unmount ZFS filesystem: %s", err)
				}
			}

			os.Remove(oldImageMntPoint)
		}

		// Set new mountpoint for the container's dataset it will be
		// automatically mounted.
		output, err := shared.RunCommand("zfs", "set", "mountpoint=none", imageDataset)
		if err != nil {
			logger.Warnf("Failed to set new ZFS mountpoint: %s.", output)
		}
	}

	var finalErr error
	if len(failedUpgradeEntities) > 0 {
		finalErr = fmt.Errorf(strings.Join(failedUpgradeEntities, "\n"))
	}

	return finalErr
}

func updatePoolPropertyForAllObjects(d *Daemon, poolName string, allcontainers []string) error {
	// The new storage api enforces that the default storage pool on which
	// containers are created is set in the default profile. If it isn't
	// set, then LXD will refuse to create a container until either an
	// appropriate device including a pool is added to the default profile
	// or the user explicitly passes the pool the container's storage volume
	// is supposed to be created on.
	profiles, err := db.Profiles(d.db)
	if err == nil {
		for _, pName := range profiles {
			pID, p, err := db.ProfileGet(d.db, pName)
			if err != nil {
				logger.Errorf("Could not query database: %s.", err)
				return err
			}

			// Check for a root disk device entry
			k, _, _ := containerGetRootDiskDevice(p.Devices)
			if k != "" {
				if p.Devices[k]["pool"] != "" {
					continue
				}

				p.Devices[k]["pool"] = poolName
			} else if k == "" && pName == "default" {
				// The default profile should have a valid root
				// disk device entry.
				rootDev := map[string]string{}
				rootDev["type"] = "disk"
				rootDev["path"] = "/"
				rootDev["pool"] = poolName
				if p.Devices == nil {
					p.Devices = map[string]map[string]string{}
				}

				// Make sure that we do not overwrite a device the user
				// is currently using under the name "root".
				rootDevName := "root"
				for i := 0; i < 100; i++ {
					if p.Devices[rootDevName] == nil {
						break
					}
					rootDevName = fmt.Sprintf("root%d", i)
					continue
				}
				p.Devices["root"] = rootDev
			}

			// This is nasty, but we need to clear the profiles config and
			// devices in order to add the new root device including the
			// newly added storage pool.
			tx, err := db.Begin(d.db)
			if err != nil {
				return err
			}

			err = db.ProfileConfigClear(tx, pID)
			if err != nil {
				logger.Errorf("Failed to clear old profile configuration for profile %s: %s.", pName, err)
				tx.Rollback()
				continue
			}

			err = db.ProfileConfigAdd(tx, pID, p.Config)
			if err != nil {
				logger.Errorf("Failed to add new profile configuration: %s: %s.", pName, err)
				tx.Rollback()
				continue
			}

			err = db.DevicesAdd(tx, "profile", pID, p.Devices)
			if err != nil {
				logger.Errorf("Failed to add new profile profile root disk device: %s: %s.", pName, err)
				tx.Rollback()
				continue
			}

			err = tx.Commit()
			if err != nil {
				logger.Errorf("Failed to commit database transaction: %s: %s.", pName, err)
				tx.Rollback()
				continue
			}
		}
	}

	// Make sure all containers and snapshots have a valid disk configuration
	for _, ct := range allcontainers {
		c, err := containerLoadByName(d.State(), ct)
		if err != nil {
			continue
		}

		args := db.ContainerArgs{
			Architecture: c.Architecture(),
			Config:       c.LocalConfig(),
			Ephemeral:    c.IsEphemeral(),
			CreationDate: c.CreationDate(),
			LastUsedDate: c.LastUsedDate(),
			Name:         c.Name(),
			Profiles:     c.Profiles(),
		}

		if c.IsSnapshot() {
			args.Ctype = db.CTypeSnapshot
		} else {
			args.Ctype = db.CTypeRegular
		}

		// Check if the container already has a valid root device entry (profile or previous upgrade)
		expandedDevices := c.ExpandedDevices()
		k, d, _ := containerGetRootDiskDevice(expandedDevices)
		if k != "" && d["pool"] != "" {
			continue
		}

		// Look for a local root device entry
		localDevices := c.LocalDevices()
		k, _, _ = containerGetRootDiskDevice(localDevices)
		if k != "" {
			localDevices[k]["pool"] = poolName
		} else {
			rootDev := map[string]string{}
			rootDev["type"] = "disk"
			rootDev["path"] = "/"
			rootDev["pool"] = poolName

			// Make sure that we do not overwrite a device the user
			// is currently using under the name "root".
			rootDevName := "root"
			for i := 0; i < 100; i++ {
				if expandedDevices[rootDevName] == nil {
					break
				}

				rootDevName = fmt.Sprintf("root%d", i)
				continue
			}

			localDevices[rootDevName] = rootDev
		}
		args.Devices = localDevices

		err = c.Update(args, false)
		if err != nil {
			continue
		}
	}

	return nil
}

func patchStorageApiV1(name string, d *Daemon) error {
	pools, err := db.StoragePools(d.db)
	if err != nil && err == db.NoSuchObjectError {
		// No pool was configured in the previous update. So we're on a
		// pristine LXD instance.
		return nil
	} else if err != nil {
		// Database is screwed.
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	if len(pools) != 1 {
		logger.Warnf("More than one storage pool found. Not rerunning upgrade.")
		return nil
	}

	cRegular, err := db.ContainersList(d.db, db.CTypeRegular)
	if err != nil {
		return err
	}

	// Get list of existing snapshots.
	cSnapshots, err := db.ContainersList(d.db, db.CTypeSnapshot)
	if err != nil {
		return err
	}

	allcontainers := append(cRegular, cSnapshots...)
	err = updatePoolPropertyForAllObjects(d, pools[0], allcontainers)
	if err != nil {
		return err
	}

	return nil
}

func patchStorageApiDirCleanup(name string, d *Daemon) error {
	_, err := db.Exec(d.db, "DELETE FROM storage_volumes WHERE type=? AND name NOT IN (SELECT fingerprint FROM images);", storagePoolVolumeTypeImage)
	if err != nil {
		return err
	}

	return nil
}

func patchStorageApiLvmKeys(name string, d *Daemon) error {
	_, err := db.Exec(d.db, "UPDATE storage_pools_config SET key='lvm.thinpool_name' WHERE key='volume.lvm.thinpool_name';")
	if err != nil {
		return err
	}

	_, err = db.Exec(d.db, "DELETE FROM storage_volumes_config WHERE key='lvm.thinpool_name';")
	if err != nil {
		return err
	}

	return nil
}

func patchStorageApiKeys(name string, d *Daemon) error {
	pools, err := db.StoragePools(d.db)
	if err != nil && err == db.NoSuchObjectError {
		// No pool was configured in the previous update. So we're on a
		// pristine LXD instance.
		return nil
	} else if err != nil {
		// Database is screwed.
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	for _, poolName := range pools {
		_, pool, err := db.StoragePoolGet(d.db, poolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// We only care about zfs and lvm.
		if pool.Driver != "zfs" && pool.Driver != "lvm" {
			continue
		}

		// This is a loop backed pool.
		if filepath.IsAbs(pool.Config["source"]) {
			continue
		}

		// Ensure that the source and the zfs.pool_name or lvm.vg_name
		// are lined up. After creation of the pool they should never
		// differ except in the loop backed case.
		if pool.Driver == "zfs" {
			pool.Config["zfs.pool_name"] = pool.Config["source"]
		} else if pool.Driver == "lvm" {
			// On previous upgrade versions, "size" was set instead
			// of "volume.size", so transfer the value and then
			// unset it.
			if pool.Config["size"] != "" {
				pool.Config["volume.size"] = pool.Config["size"]
				pool.Config["size"] = ""
			}
			pool.Config["lvm.vg_name"] = pool.Config["source"]
		}

		// Update the config in the database.
		err = db.StoragePoolUpdate(d.db, poolName, pool.Description, pool.Config)
		if err != nil {
			return err
		}
	}

	return nil
}

// In case any of the objects images/containers/snapshots are missing storage
// volume configuration entries, let's add the defaults.
func patchStorageApiUpdateStorageConfigs(name string, d *Daemon) error {
	pools, err := db.StoragePools(d.db)
	if err != nil {
		if err == db.NoSuchObjectError {
			return nil
		}
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	for _, poolName := range pools {
		poolID, pool, err := db.StoragePoolGet(d.db, poolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// Make sure that config is not empty.
		if pool.Config == nil {
			pool.Config = map[string]string{}
		}

		// Insert default values.
		err = storagePoolFillDefault(poolName, pool.Driver, pool.Config)
		if err != nil {
			return err
		}

		// Manually check for erroneously set keys.
		switch pool.Driver {
		case "btrfs":
			// Unset "size" property on non loop-backed pools.
			if pool.Config["size"] != "" {
				// Unset if either not an absolute path or not a
				// loop file.
				if !filepath.IsAbs(pool.Config["source"]) ||
					(filepath.IsAbs(pool.Config["source"]) &&
						!strings.HasSuffix(pool.Config["source"], ".img")) {
					pool.Config["size"] = ""
				}
			}
		case "dir":
			// Unset "size" property for all dir backed pools.
			if pool.Config["size"] != "" {
				pool.Config["size"] = ""
			}
		case "lvm":
			// Unset "size" property for volume-group level.
			if pool.Config["size"] != "" {
				pool.Config["size"] = ""
			}

			// Unset default values.
			if pool.Config["volume.block.mount_options"] == "discard" {
				pool.Config["volume.block.mount_options"] = ""
			}

			if pool.Config["volume.block.filesystem"] == "ext4" {
				pool.Config["volume.block.filesystem"] = ""
			}
		case "zfs":
			// Unset default values.
			if !shared.IsTrue(pool.Config["volume.zfs.use_refquota"]) {
				pool.Config["volume.zfs.use_refquota"] = ""
			}

			if !shared.IsTrue(pool.Config["volume.zfs.remove_snapshots"]) {
				pool.Config["volume.zfs.remove_snapshots"] = ""
			}

			// Unset "size" property on non loop-backed pools.
			if pool.Config["size"] != "" && !filepath.IsAbs(pool.Config["source"]) {
				pool.Config["size"] = ""
			}
		}

		// Update the storage pool config.
		err = db.StoragePoolUpdate(d.db, poolName, pool.Description, pool.Config)
		if err != nil {
			return err
		}

		// Get all storage volumes on the storage pool.
		volumes, err := db.StoragePoolVolumesGet(d.db, poolID, supportedVolumeTypes)
		if err != nil {
			if err == db.NoSuchObjectError {
				continue
			}
			return err
		}

		for _, volume := range volumes {
			// Make sure that config is not empty.
			if volume.Config == nil {
				volume.Config = map[string]string{}
			}

			// Insert default values.
			err := storageVolumeFillDefault(volume.Name, volume.Config, pool)
			if err != nil {
				return err
			}

			// Manually check for erroneously set keys.
			switch pool.Driver {
			case "btrfs":
				// Unset "size" property.
				if volume.Config["size"] != "" {
					volume.Config["size"] = ""
				}
			case "dir":
				// Unset "size" property for all dir backed pools.
				if volume.Config["size"] != "" {
					volume.Config["size"] = ""
				}
			case "lvm":
				// Unset default values.
				if volume.Config["block.mount_options"] == "discard" {
					volume.Config["block.mount_options"] = ""
				}
			case "zfs":
				// Unset default values.
				if !shared.IsTrue(volume.Config["zfs.use_refquota"]) {
					volume.Config["zfs.use_refquota"] = ""
				}
				if !shared.IsTrue(volume.Config["zfs.remove_snapshots"]) {
					volume.Config["zfs.remove_snapshots"] = ""
				}
				// Unset "size" property.
				if volume.Config["size"] != "" {
					volume.Config["size"] = ""
				}
			}

			// It shouldn't be possible that false volume types
			// exist in the db, so it's safe to ignore the error.
			volumeType, _ := storagePoolVolumeTypeNameToType(volume.Type)
			// Update the volume config.
			err = db.StoragePoolVolumeUpdate(d.db, volume.Name, volumeType, poolID, volume.Description, volume.Config)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func patchStorageApiLxdOnBtrfs(name string, d *Daemon) error {
	pools, err := db.StoragePools(d.db)
	if err != nil {
		if err == db.NoSuchObjectError {
			return nil
		}
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	for _, poolName := range pools {
		_, pool, err := db.StoragePoolGet(d.db, poolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// Make sure that config is not empty.
		if pool.Config == nil {
			pool.Config = map[string]string{}

			// Insert default values.
			err = storagePoolFillDefault(poolName, pool.Driver, pool.Config)
			if err != nil {
				return err
			}
		}

		if d.os.BackingFS != "btrfs" {
			continue
		}

		if pool.Driver != "btrfs" {
			continue
		}

		source := pool.Config["source"]
		cleanSource := filepath.Clean(source)
		loopFilePath := shared.VarPath("disks", poolName+".img")
		if cleanSource != loopFilePath {
			continue
		}

		pool.Config["source"] = getStoragePoolMountPoint(poolName)

		// Update the storage pool config.
		err = db.StoragePoolUpdate(d.db, poolName, pool.Description, pool.Config)
		if err != nil {
			return err
		}

		os.Remove(loopFilePath)
	}

	return nil
}

func patchStorageApiDetectLVSize(name string, d *Daemon) error {
	pools, err := db.StoragePools(d.db)
	if err != nil {
		if err == db.NoSuchObjectError {
			return nil
		}
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	for _, poolName := range pools {
		poolID, pool, err := db.StoragePoolGet(d.db, poolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// Make sure that config is not empty.
		if pool.Config == nil {
			pool.Config = map[string]string{}

			// Insert default values.
			err = storagePoolFillDefault(poolName, pool.Driver, pool.Config)
			if err != nil {
				return err
			}
		}

		// We're only interested in LVM pools.
		if pool.Driver != "lvm" {
			continue
		}

		// Get all storage volumes on the storage pool.
		volumes, err := db.StoragePoolVolumesGet(d.db, poolID, supportedVolumeTypes)
		if err != nil {
			if err == db.NoSuchObjectError {
				continue
			}
			return err
		}

		poolName := pool.Config["lvm.vg_name"]
		if poolName == "" {
			logger.Errorf("The \"lvm.vg_name\" key should not be empty.")
			return fmt.Errorf("The \"lvm.vg_name\" key should not be empty.")
		}

		for _, volume := range volumes {
			// Make sure that config is not empty.
			if volume.Config == nil {
				volume.Config = map[string]string{}

				// Insert default values.
				err := storageVolumeFillDefault(volume.Name, volume.Config, pool)
				if err != nil {
					return err
				}
			}

			// It shouldn't be possible that false volume types
			// exist in the db, so it's safe to ignore the error.
			volumeTypeApiEndpoint, _ := storagePoolVolumeTypeNameToAPIEndpoint(volume.Type)
			lvmName := containerNameToLVName(volume.Name)
			lvmLvDevPath := getLvmDevPath(poolName, volumeTypeApiEndpoint, lvmName)
			size, err := lvmGetLVSize(lvmLvDevPath)
			if err != nil {
				logger.Errorf("Failed to detect size of logical volume: %s.", err)
				return err
			}

			if volume.Config["size"] == size {
				continue
			}

			volume.Config["size"] = size

			// It shouldn't be possible that false volume types
			// exist in the db, so it's safe to ignore the error.
			volumeType, _ := storagePoolVolumeTypeNameToType(volume.Type)
			// Update the volume config.
			err = db.StoragePoolVolumeUpdate(d.db, volume.Name, volumeType, poolID, volume.Description, volume.Config)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func patchStorageApiInsertZfsDriver(name string, d *Daemon) error {
	_, err := db.Exec(d.db, "UPDATE storage_pools SET driver='zfs', description='' WHERE driver=''")
	if err != nil {
		return err
	}

	return nil
}

func patchStorageZFSnoauto(name string, d *Daemon) error {
	pools, err := db.StoragePools(d.db)
	if err != nil {
		if err == db.NoSuchObjectError {
			return nil
		}
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	for _, poolName := range pools {
		_, pool, err := db.StoragePoolGet(d.db, poolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		if pool.Driver != "zfs" {
			continue
		}

		zpool := pool.Config["zfs.pool_name"]
		if zpool == "" {
			continue
		}

		containersDatasetPath := fmt.Sprintf("%s/containers", zpool)
		customDatasetPath := fmt.Sprintf("%s/custom", zpool)
		paths := []string{}
		for _, v := range []string{containersDatasetPath, customDatasetPath} {
			_, err := shared.RunCommand("zfs", "get", "-H", "-p", "-o", "value", "name", v)
			if err == nil {
				paths = append(paths, v)
			}
		}

		args := []string{"list", "-t", "filesystem", "-o", "name", "-H", "-r"}
		args = append(args, paths...)

		output, err := shared.RunCommand("zfs", args...)
		if err != nil {
			return fmt.Errorf("Unable to list containers on zpool: %s", zpool)
		}

		for _, entry := range strings.Split(output, "\n") {
			if entry == "" {
				continue
			}

			if shared.StringInSlice(entry, paths) {
				continue
			}

			_, err := shared.RunCommand("zfs", "set", "canmount=noauto", entry)
			if err != nil {
				return fmt.Errorf("Unable to set canmount=noauto on: %s", entry)
			}
		}
	}

	return nil
}

func patchStorageZFSVolumeSize(name string, d *Daemon) error {
	pools, err := db.StoragePools(d.db)
	if err != nil && err == db.NoSuchObjectError {
		// No pool was configured in the previous update. So we're on a
		// pristine LXD instance.
		return nil
	} else if err != nil {
		// Database is screwed.
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	for _, poolName := range pools {
		poolID, pool, err := db.StoragePoolGet(d.db, poolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// We only care about zfs
		if pool.Driver != "zfs" {
			continue
		}

		// Get all storage volumes on the storage pool.
		volumes, err := db.StoragePoolVolumesGet(d.db, poolID, supportedVolumeTypes)
		if err != nil {
			if err == db.NoSuchObjectError {
				continue
			}
			return err
		}

		for _, volume := range volumes {
			if volume.Type != "container" && volume.Type != "image" {
				continue
			}

			// ZFS storage volumes for containers and images should
			// never have a size property set directly on the
			// storage volume itself. For containers the size
			// property is regulated either via a profiles root disk
			// device size property or via the containers local
			// root disk device size property. So unset it here
			// unconditionally.
			if volume.Config["size"] != "" {
				volume.Config["size"] = ""
			}

			// It shouldn't be possible that false volume types
			// exist in the db, so it's safe to ignore the error.
			volumeType, _ := storagePoolVolumeTypeNameToType(volume.Type)
			// Update the volume config.
			err = db.StoragePoolVolumeUpdate(d.db, volume.Name,
				volumeType, poolID, volume.Description,
				volume.Config)
			if err != nil {
				return err
			}
		}

	}

	return nil
}

func patchNetworkDnsmasqHosts(name string, d *Daemon) error {
	// Get the list of networks
	networks, err := db.Networks(d.db)
	if err != nil {
		return err
	}

	for _, network := range networks {
		// Remove the old dhcp-hosts file (will be re-generated on startup)
		if shared.PathExists(shared.VarPath("networks", network, "dnsmasq.hosts")) {
			err = os.Remove(shared.VarPath("networks", network, "dnsmasq.hosts"))
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func patchStorageApiDirBindMount(name string, d *Daemon) error {
	pools, err := db.StoragePools(d.db)
	if err != nil && err == db.NoSuchObjectError {
		// No pool was configured in the previous update. So we're on a
		// pristine LXD instance.
		return nil
	} else if err != nil {
		// Database is screwed.
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	for _, poolName := range pools {
		_, pool, err := db.StoragePoolGet(d.db, poolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// We only care about dir
		if pool.Driver != "dir" {
			continue
		}

		source := pool.Config["source"]
		if source == "" {
			msg := fmt.Sprintf(`No "source" property for storage `+
				`pool "%s" found`, poolName)
			logger.Errorf(msg)
			return fmt.Errorf(msg)
		}
		cleanSource := filepath.Clean(source)
		poolMntPoint := getStoragePoolMountPoint(poolName)

		if cleanSource == poolMntPoint {
			continue
		}

		if shared.PathExists(poolMntPoint) {
			err := os.Remove(poolMntPoint)
			if err != nil {
				return err
			}
		}

		err = os.MkdirAll(poolMntPoint, 0711)
		if err != nil {
			return err
		}

		mountSource := cleanSource
		mountFlags := syscall.MS_BIND

		err = syscall.Mount(mountSource, poolMntPoint, "", uintptr(mountFlags), "")
		if err != nil {
			logger.Errorf(`Failed to mount DIR storage pool "%s" onto `+
				`"%s": %s`, mountSource, poolMntPoint, err)
			return err
		}

	}

	return nil
}

func patchFixUploadedAt(name string, d *Daemon) error {
	images, err := db.ImagesGet(d.db, false)
	if err != nil {
		return err
	}

	for _, fingerprint := range images {
		id, image, err := db.ImageGet(d.db, fingerprint, false, true)
		if err != nil {
			return err
		}

		_, err = db.Exec(d.db, "UPDATE images SET upload_date=? WHERE id=?", image.UploadedAt, id)
		if err != nil {
			return err
		}
	}

	return nil
}

func patchStorageApiCephSizeRemove(name string, d *Daemon) error {
	pools, err := db.StoragePools(d.db)
	if err != nil && err == db.NoSuchObjectError {
		// No pool was configured in the previous update. So we're on a
		// pristine LXD instance.
		return nil
	} else if err != nil {
		// Database is screwed.
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	for _, poolName := range pools {
		_, pool, err := db.StoragePoolGet(d.db, poolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// We only care about zfs and lvm.
		if pool.Driver != "ceph" {
			continue
		}

		// The "size" property does not make sense for ceph osd storage pools.
		if pool.Config["size"] != "" {
			pool.Config["size"] = ""
		}

		// Update the config in the database.
		err = db.StoragePoolUpdate(d.db, poolName, pool.Description,
			pool.Config)
		if err != nil {
			return err
		}
	}

	return nil
}

// Patches end here

// Here are a couple of legacy patches that were originally in
// db_updates.go and were written before the new patch mechanism
// above. To preserve exactly their semantics we treat them
// differently and still apply them during the database upgrade. In
// principle they could be converted to regular patches like the ones
// above, however that seems an unnecessary risk at this moment. See
// also PR #3322.
//
// NOTE: don't add any legacy patch here, instead use the patches
// mechanism above.
var legacyPatches = map[int](func(d *Daemon) error){
	11: patchUpdateFromV10,
	12: patchUpdateFromV11,
	16: patchUpdateFromV15,
	30: patchUpdateFromV29,
	31: patchUpdateFromV30,
}
var legacyPatchesNeedingDB = []int{11, 12, 16} // Legacy patches doing DB work

func patchUpdateFromV10(d *Daemon) error {
	if shared.PathExists(shared.VarPath("lxc")) {
		err := os.Rename(shared.VarPath("lxc"), shared.VarPath("containers"))
		if err != nil {
			return err
		}

		logger.Debugf("Restarting all the containers following directory rename")
		s := d.State()
		containersShutdown(s)
		containersRestart(s)
	}

	return nil
}

func patchUpdateFromV11(d *Daemon) error {
	cNames, err := db.ContainersList(d.db, db.CTypeSnapshot)
	if err != nil {
		return err
	}

	errors := 0

	for _, cName := range cNames {
		snapParentName, snapOnlyName, _ := containerGetParentAndSnapshotName(cName)
		oldPath := shared.VarPath("containers", snapParentName, "snapshots", snapOnlyName)
		newPath := shared.VarPath("snapshots", snapParentName, snapOnlyName)
		if shared.PathExists(oldPath) && !shared.PathExists(newPath) {
			logger.Info(
				"Moving snapshot",
				log.Ctx{
					"snapshot": cName,
					"oldPath":  oldPath,
					"newPath":  newPath})

			// Rsync
			// containers/<container>/snapshots/<snap0>
			// to
			// snapshots/<container>/<snap0>
			output, err := rsyncLocalCopy(oldPath, newPath, "")
			if err != nil {
				logger.Error(
					"Failed rsync snapshot",
					log.Ctx{
						"snapshot": cName,
						"output":   string(output),
						"err":      err})
				errors++
				continue
			}

			// Remove containers/<container>/snapshots/<snap0>
			if err := os.RemoveAll(oldPath); err != nil {
				logger.Error(
					"Failed to remove the old snapshot path",
					log.Ctx{
						"snapshot": cName,
						"oldPath":  oldPath,
						"err":      err})

				// Ignore this error.
				// errors++
				// continue
			}

			// Remove /var/lib/lxd/containers/<container>/snapshots
			// if its empty.
			cPathParent := filepath.Dir(oldPath)
			if ok, _ := shared.PathIsEmpty(cPathParent); ok {
				os.Remove(cPathParent)
			}

		} // if shared.PathExists(oldPath) && !shared.PathExists(newPath) {
	} // for _, cName := range cNames {

	// Refuse to start lxd if a rsync failed.
	if errors > 0 {
		return fmt.Errorf("Got errors while moving snapshots, see the log output.")
	}

	return nil
}

func patchUpdateFromV15(d *Daemon) error {
	// munge all LVM-backed containers' LV names to match what is
	// required for snapshot support

	cNames, err := db.ContainersList(d.db, db.CTypeRegular)
	if err != nil {
		return err
	}

	err = daemonConfigInit(d.db)
	if err != nil {
		return err
	}

	vgName := daemonConfig["storage.lvm_vg_name"].Get()

	for _, cName := range cNames {
		var lvLinkPath string
		if strings.Contains(cName, shared.SnapshotDelimiter) {
			lvLinkPath = shared.VarPath("snapshots", fmt.Sprintf("%s.lv", cName))
		} else {
			lvLinkPath = shared.VarPath("containers", fmt.Sprintf("%s.lv", cName))
		}

		if !shared.PathExists(lvLinkPath) {
			continue
		}

		newLVName := strings.Replace(cName, "-", "--", -1)
		newLVName = strings.Replace(newLVName, shared.SnapshotDelimiter, "-", -1)

		if cName == newLVName {
			logger.Debug("No need to rename, skipping", log.Ctx{"cName": cName, "newLVName": newLVName})
			continue
		}

		logger.Debug("About to rename cName in lv upgrade", log.Ctx{"lvLinkPath": lvLinkPath, "cName": cName, "newLVName": newLVName})

		output, err := shared.RunCommand("lvrename", vgName, cName, newLVName)
		if err != nil {
			return fmt.Errorf("Could not rename LV '%s' to '%s': %v\noutput:%s", cName, newLVName, err, output)
		}

		if err := os.Remove(lvLinkPath); err != nil {
			return fmt.Errorf("Couldn't remove lvLinkPath '%s'", lvLinkPath)
		}
		newLinkDest := fmt.Sprintf("/dev/%s/%s", vgName, newLVName)
		if err := os.Symlink(newLinkDest, lvLinkPath); err != nil {
			return fmt.Errorf("Couldn't recreate symlink '%s'->'%s'", lvLinkPath, newLinkDest)
		}
	}

	return nil
}

func patchUpdateFromV29(d *Daemon) error {
	if shared.PathExists(shared.VarPath("zfs.img")) {
		err := os.Chmod(shared.VarPath("zfs.img"), 0600)
		if err != nil {
			return err
		}
	}

	return nil
}

func patchUpdateFromV30(d *Daemon) error {
	entries, err := ioutil.ReadDir(shared.VarPath("containers"))
	if err != nil {
		/* If the directory didn't exist before, the user had never
		 * started containers, so we don't need to fix up permissions
		 * on anything.
		 */
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if !shared.IsDir(shared.VarPath("containers", entry.Name(), "rootfs")) {
			continue
		}

		info, err := os.Stat(shared.VarPath("containers", entry.Name(), "rootfs"))
		if err != nil {
			return err
		}

		if int(info.Sys().(*syscall.Stat_t).Uid) == 0 {
			err := os.Chmod(shared.VarPath("containers", entry.Name()), 0700)
			if err != nil {
				return err
			}

			err = os.Chown(shared.VarPath("containers", entry.Name()), 0, 0)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
