package storage

import (
	"fmt"

	"github.com/lxc/lxd/lxd/cluster/request"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/lxd/storage/drivers"
	"github.com/lxc/lxd/shared/api"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/logging"
)

// volIDFuncMake returns a function that can be supplied to the underlying storage drivers allowing
// them to lookup the volume ID for a specific volume type and volume name. This function is tied
// to the Pool ID that it is generated for, meaning the storage drivers do not need to know the ID
// of the pool they belong to, or do they need access to the database.
func volIDFuncMake(state *state.State, poolID int64) func(volType drivers.VolumeType, volName string) (int64, error) {
	// Return a function to retrieve a volume ID for a volume Name for use in driver.
	return func(volType drivers.VolumeType, volName string) (int64, error) {
		volTypeID, err := VolumeTypeToDBType(volType)
		if err != nil {
			return -1, err
		}

		// It is possible for the project name to be encoded into the volume name in the
		// format <project>_<volume>. However not all volume types currently use this
		// encoding format, so if there is no underscore in the volume name then we assume
		// the project is default.
		projectName := project.Default

		// Currently only Containers, VMs and custom volumes support project level volumes.
		// This means that other volume types may have underscores in their names that don't
		// indicate the project name.
		if volType == drivers.VolumeTypeContainer || volType == drivers.VolumeTypeVM {
			projectName, volName = project.InstanceParts(volName)
		} else if volType == drivers.VolumeTypeCustom {
			projectName, volName = project.StorageVolumeParts(volName)
		}

		volID, _, err := state.Cluster.GetLocalStoragePoolVolume(projectName, volName, volTypeID, poolID)
		if err != nil {
			if err == db.ErrNoSuchObject {
				return -1, fmt.Errorf("Failed to get volume ID for project %q, volume %q, type %q: Volume doesn't exist", projectName, volName, volType)
			}

			return -1, err
		}

		return volID, nil
	}
}

// commonRules returns a set of common validators.
func commonRules() *drivers.Validators {
	return &drivers.Validators{
		PoolRules:   validatePoolCommonRules,
		VolumeRules: validateVolumeCommonRules,
	}
}

// New instantiates a pool from config supplied and returns a Pool interface.
// If the pool's driver is not recognised then drivers.ErrUnknownDriver is returned.
func New(state *state.State, info *api.StoragePool) (Pool, error) {
	// Handle mock requests.
	if state.OS.MockMode {
		pool := mockBackend{}
		pool.name = info.Name
		pool.state = state
		pool.logger = logging.AddContext(logger.Log, log.Ctx{"driver": "mock", "pool": pool.name})
		driver, err := drivers.Load(state, "mock", "", nil, pool.logger, nil, nil)
		if err != nil {
			return nil, err
		}
		pool.driver = driver

		return &pool, nil
	}

	var poolID int64 = -1 // No DB ID as not from database. Not all functionality will be available.

	// Ensure a config map exists.
	if info.Config == nil {
		info.Config = map[string]string{}
	}

	logger := logging.AddContext(logger.Log, log.Ctx{"driver": info.Driver, "pool": info.Name})

	// Load the storage driver.
	driver, err := drivers.Load(state, info.Driver, info.Name, info.Config, logger, volIDFuncMake(state, poolID), commonRules())
	if err != nil {
		return nil, err
	}

	// Setup the pool struct.
	pool := lxdBackend{}
	pool.driver = driver
	pool.id = poolID
	pool.db = *info
	pool.name = info.Name
	pool.state = state
	pool.logger = logger
	pool.nodes = nil // TODO support clustering.

	return &pool, nil
}

// CreatePool creates a new storage pool on disk and returns a Pool interface.
// If the pool's driver is not recognised then drivers.ErrUnknownDriver is returned.
// Deprecated, used only by patches.
func CreatePool(state *state.State, poolID int64, dbPool *api.StoragePoolsPost) (Pool, error) {
	// Quick checks.
	if dbPool == nil {
		return nil, ErrNilValue
	}

	// Ensure a config map exists.
	if dbPool.Config == nil {
		dbPool.Config = map[string]string{}
	}

	// Handle mock requests.
	if state.OS.MockMode {
		pool := mockBackend{}
		pool.name = dbPool.Name
		pool.state = state
		pool.logger = logging.AddContext(logger.Log, log.Ctx{"driver": "mock", "pool": pool.name})
		driver, err := drivers.Load(state, "mock", "", nil, pool.logger, nil, nil)
		if err != nil {
			return nil, err
		}
		pool.driver = driver

		return &pool, nil
	}

	logger := logging.AddContext(logger.Log, log.Ctx{"driver": dbPool.Driver, "pool": dbPool.Name})

	// Load the storage driver.
	driver, err := drivers.Load(state, dbPool.Driver, dbPool.Name, dbPool.Config, logger, volIDFuncMake(state, poolID), commonRules())
	if err != nil {
		return nil, err
	}

	// Setup the pool struct.
	pool := lxdBackend{}
	pool.driver = driver
	pool.id = poolID
	pool.db = api.StoragePool{
		StoragePoolPut: dbPool.StoragePoolPut,
		Name:           dbPool.Name,
		Driver:         dbPool.Driver,
	}
	pool.name = dbPool.Name
	pool.state = state
	pool.logger = logger
	pool.nodes = map[int64]db.StoragePoolNode{} // Nodes unknown at this point.

	// Create the pool itself on the storage device..
	err = pool.Create(request.ClientTypeNormal, nil)
	if err != nil {
		return nil, err
	}

	return &pool, nil
}

// GetPoolByName retrieves the pool from the database by its name and returns a Pool interface.
// If the pool's driver is not recognised then drivers.ErrUnknownDriver is returned.
func GetPoolByName(state *state.State, name string) (Pool, error) {
	// Handle mock requests.
	if state.OS.MockMode {
		pool := mockBackend{}
		pool.name = name
		pool.state = state
		pool.logger = logging.AddContext(logger.Log, log.Ctx{"driver": "mock", "pool": pool.name})
		driver, err := drivers.Load(state, "mock", "", nil, pool.logger, nil, nil)
		if err != nil {
			return nil, err
		}
		pool.driver = driver

		return &pool, nil
	}

	// Load the database record.
	poolID, dbPool, poolNodes, err := state.Cluster.GetStoragePoolInAnyState(name)
	if err != nil {
		return nil, err
	}

	// Ensure a config map exists.
	if dbPool.Config == nil {
		dbPool.Config = map[string]string{}
	}

	logger := logging.AddContext(logger.Log, log.Ctx{"driver": dbPool.Driver, "pool": dbPool.Name})

	// Load the storage driver.
	driver, err := drivers.Load(state, dbPool.Driver, dbPool.Name, dbPool.Config, logger, volIDFuncMake(state, poolID), commonRules())
	if err != nil {
		return nil, err
	}

	// Setup the pool struct.
	pool := lxdBackend{}
	pool.driver = driver
	pool.id = poolID
	pool.db = *dbPool
	pool.name = dbPool.Name
	pool.state = state
	pool.logger = logger
	pool.nodes = poolNodes

	return &pool, nil
}

// GetPoolByInstance retrieves the pool from the database using the instance's pool.
// If the pool's driver is not recognised then drivers.ErrUnknownDriver is returned. If the pool's
// driver does not support the instance's type then drivers.ErrNotSupported is returned.
func GetPoolByInstance(s *state.State, inst instance.Instance) (Pool, error) {
	poolName, err := s.Cluster.GetInstancePool(inst.Project(), inst.Name())
	if err != nil {
		return nil, err
	}

	pool, err := GetPoolByName(s, poolName)
	if err != nil {
		return nil, err
	}

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return nil, err
	}

	for _, supportedType := range pool.Driver().Info().VolumeTypes {
		if supportedType == volType {
			return pool, nil
		}
	}

	// Return drivers not supported error for consistency with predefined errors returned by
	// GetPoolByName (which can return drivers.ErrUnknownDriver).
	return nil, drivers.ErrNotSupported
}
