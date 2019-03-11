package foundationdbcluster

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"reflect"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/google/uuid"
	fdbtypes "github.com/brownleej/fdb-kubernetes-operator/pkg/apis/apps/v1beta1"
)

var configurationProtocolVersion = []byte("\x01\x00\x04Q\xa5\x00\xdb\x0f")

// AdminClient describes an interface for running administrative commands on a
// cluster
type AdminClient interface {
	// ConfigureDatabase sets the database configuration
	ConfigureDatabase(configuration DatabaseConfiguration, newDatabase bool) error

	// ExcludeInstances starts evacuating processes so that they can be removed
	// from the database.
	ExcludeInstances(addresses []string) error

	// IncludeInstances removes processes from the exclusion list and allows
	// them to take on roles again.
	IncludeInstances(addresses []string) error

	// CanSafelyRemove checks whether it is safe to remove processes from the
	// cluster
	CanSafelyRemove(addresses []string) ([]string, error)
}

// DatabaseConfiguration represents the desired
type DatabaseConfiguration struct {
	ReplicationMode string
	StorageEngine   string
}

func (configuration DatabaseConfiguration) getConfigurationKeys() ([]fdb.KeyValue, error) {
	keys := make([]fdb.KeyValue, 0)
	var policy localityPolicy
	var replicas []byte

	switch configuration.ReplicationMode {
	case "single":
		policy = &singletonPolicy{}
		replicas = []byte("1")
	case "double":
		policy = &acrossPolicy{
			Count:     2,
			Field:     "zoneid",
			Subpolicy: &singletonPolicy{},
		}
		replicas = []byte("2")
	case "triple":
		policy = &acrossPolicy{
			Count:     3,
			Field:     "zoneid",
			Subpolicy: &singletonPolicy{},
		}
		replicas = []byte("3")
	default:
		return nil, fmt.Errorf("Unknown replication mode %s", configuration.ReplicationMode)
	}

	policyBytes := bytes.Join([][]byte{configurationProtocolVersion, policy.BinaryRepresentation()}, nil)
	keys = append(keys,
		fdb.KeyValue{Key: fdb.Key("\xff/conf/storage_replicas"), Value: replicas},
		fdb.KeyValue{Key: fdb.Key("\xff/conf/log_replicas"), Value: replicas},
		fdb.KeyValue{Key: fdb.Key("\xff/conf/log_anti_quorum"), Value: []byte("0")},
		fdb.KeyValue{Key: fdb.Key("\xff/conf/storage_replication_policy"), Value: policyBytes},
		fdb.KeyValue{Key: fdb.Key("\xff/conf/log_replication_policy"), Value: policyBytes},
	)

	var engine []byte
	switch configuration.StorageEngine {
	case "ssd":
		engine = []byte("1")
	case "memory":
		engine = []byte("2")
	default:
		return nil, fmt.Errorf("Unknown storage engine %s", configuration.StorageEngine)
	}

	keys = append(keys,
		fdb.KeyValue{Key: fdb.Key("\xff/conf/storage_engine"), Value: engine},
		fdb.KeyValue{Key: fdb.Key("\xff/conf/log_engine"), Value: engine},
	)
	return keys, nil
}

// RealAdminClient provides an implementation of the admin interface using the
// FDB client library
type RealAdminClient struct {
	Cluster  *fdbtypes.FoundationDBCluster
	Database fdb.Database
}

// NewAdminClient generates an Admin client for a cluster
func NewAdminClient(cluster *fdbtypes.FoundationDBCluster) (AdminClient, error) {
	err := os.MkdirAll("/tmp/fdb", os.ModePerm)
	if err != nil {
		return nil, err
	}
	clusterFilePath := fmt.Sprintf("/tmp/fdb/%s.cluster", cluster.Name)

	clusterFile, err := os.OpenFile(clusterFilePath, os.O_WRONLY|os.O_CREATE, os.ModePerm)
	if err != nil {
		return nil, err
	}
	_, err = clusterFile.WriteString(cluster.Spec.ConnectionString)
	if err != nil {
		return nil, err
	}
	err = clusterFile.Close()
	if err != nil {
		return nil, err
	}

	db, err := fdb.Open(clusterFilePath, []byte("DB"))
	if err != nil {
		return nil, err
	}

	return &RealAdminClient{Cluster: cluster, Database: db}, nil
}

// ConfigureDatabase sets the database configuration
func (client *RealAdminClient) ConfigureDatabase(configuration DatabaseConfiguration, newDatabase bool) error {

	tr, err := client.Database.CreateTransaction()
	if err != nil {
		return err
	}

	initID, err := uuid.NewRandom()
	if err != nil {
		return err
	}

	for {
		err = configureDatabaseInTransaction(configuration, newDatabase, tr, initID)
		if err == nil {
			return err
		}

		fdbErr, isFdb := err.(fdb.Error)
		if !isFdb {
			return err
		}
		if newDatabase && (fdbErr.Code == 1020 || fdbErr.Code == 1007) {
			tr.Reset()
			for {
				err := checkConfigurationInitID(tr, initID)
				if err == nil {
					return err
				}
				fdbErr, isFdb = err.(fdb.Error)
				if !isFdb {
					return fdbErr
				}
				err = tr.OnError(fdbErr).Get()
				if err != nil {
					return err
				}
			}
		} else {
			err = tr.OnError(fdbErr).Get()
			if err != nil {
				return err
			}
		}
	}
}

/**
configureDatabaseInTransaction runs the logic to change database
configuration within a transactional block.
*/
func configureDatabaseInTransaction(configuration DatabaseConfiguration, newDatabase bool, tr fdb.Transaction, initID uuid.UUID) error {
	err := tr.Options().SetAccessSystemKeys()
	if err != nil {
		return err
	}
	err = tr.Options().SetLockAware()
	if err != nil {
		return err
	}
	err = tr.Options().SetPrioritySystemImmediate()
	if err != nil {
		return err
	}
	keys, err := configuration.getConfigurationKeys()
	if err != nil {
		return err
	}
	if newDatabase {
		err = tr.Options().SetInitializeNewDatabase()
		if err != nil {
			return err
		}
		initIDKey := fdb.Key("\xff/init_id")
		err = tr.AddReadConflictKey(initIDKey)
		if err != nil {
			return err
		}

		tr.Set(fdb.Key(initIDKey), initID[:])
		tr.Set(fdb.Key("\xff/conf/initialized"), []byte("1"))
	} else {
		err = tr.Options().SetCausalWriteRisky()
		if err != nil {
			return err
		}
	}

	for _, keyValue := range keys {
		var match bool
		if !newDatabase {
			currentValue, err := tr.Get(keyValue.Key).Get()
			if err != nil {
				return err
			}
			match = reflect.DeepEqual(currentValue, keyValue.Value)
		}
		if !match {
			tr.Set(keyValue.Key, keyValue.Value)
		}
	}

	return tr.Commit().Get()
}

/**
checkConfigurationInitID is run after a transaction to create a new database
fails. It checks to see if the initial ID for the configuration is set to the
value that this transaction was trying to set.
*/
func checkConfigurationInitID(tr fdb.Transaction, initID uuid.UUID) error {
	err := tr.Options().SetPrioritySystemImmediate()
	if err != nil {
		return err
	}
	err = tr.Options().SetLockAware()
	if err != nil {
		return err
	}
	err = tr.Options().SetReadSystemKeys()
	if err != nil {
		return err
	}
	currentID, err := tr.Get(fdb.Key("\xff/init_id")).Get()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(currentID, initID[:]) {
		return errors.New("Database has already been created")
	}

	return nil
}

// ExcludeInstances starts evacuating processes so that they can be removed
// from the database.
func (client *RealAdminClient) ExcludeInstances(addresses []string) error {
	_, err := client.Database.Transact(func(tr fdb.Transaction) (interface{}, error) {
		exclusionID, err := uuid.NewRandom()
		if err != nil {
			return nil, err
		}
		err = tr.Options().SetPrioritySystemImmediate()
		if err != nil {
			return nil, err
		}

		err = tr.Options().SetAccessSystemKeys()
		if err != nil {
			return nil, err
		}

		err = tr.Options().SetLockAware()
		if err != nil {
			return nil, err
		}

		tr.AddReadConflictKey(fdb.Key("\xff/conf/excluded"))
		tr.Set(fdb.Key("\xff/conf/excluded"), exclusionID[:])
		for _, address := range addresses {
			tr.Set(
				fdb.Key(bytes.Join([][]byte{
					[]byte("\xff/conf/excluded/"),
					[]byte(address),
				}, nil)),
				nil,
			)
		}
		return nil, nil
	})
	return err
}

// IncludeInstances removes processes from the exclusion list and allows
// them to take on roles again.
func (client *RealAdminClient) IncludeInstances(addresses []string) error {
	_, err := client.Database.Transact(func(tr fdb.Transaction) (interface{}, error) {
		exclusionID, err := uuid.NewRandom()
		if err != nil {
			return nil, err
		}
		err = tr.Options().SetPrioritySystemImmediate()
		if err != nil {
			return nil, err
		}

		err = tr.Options().SetAccessSystemKeys()
		if err != nil {
			return nil, err
		}

		err = tr.Options().SetLockAware()
		if err != nil {
			return nil, err
		}

		tr.AddReadConflictKey(fdb.Key("\xff/conf/excluded"))
		tr.Set(fdb.Key("\xff/conf/excluded"), exclusionID[:])
		for _, address := range addresses {
			// Clear an exclusion on this address
			key := bytes.Join([][]byte{
				[]byte("\xff/conf/excluded/"),
				[]byte(address),
			}, nil)
			tr.Clear(fdb.Key(key))

			// Clear an exclusion on any address that starts with this address,
			// followed by a colon
			key = append(key, 58)
			keyRange, err := fdb.PrefixRange(key)
			if err != nil {
				return nil, err
			}
			tr.ClearRange(keyRange)
		}
		return nil, nil
	})
	return err
}

// CanSafelyRemove checks whether it is safe to remove processes from the
// cluster
func (client *RealAdminClient) CanSafelyRemove(addresses []string) ([]string, error) {
	return nil, nil
}

// MockAdminClient provides a mock implementation of the cluster admin interface
type MockAdminClient struct {
	Cluster *fdbtypes.FoundationDBCluster
	DatabaseConfiguration
	ExcludedAddresses   []string
	ReincludedAddresses []string
}

var adminClientCache = make(map[string]*MockAdminClient)

// NewMockAdminClient creates an admin client for a cluster.
func NewMockAdminClient(cluster *fdbtypes.FoundationDBCluster) (AdminClient, error) {
	return newMockAdminClientUncast(cluster)
}

func newMockAdminClientUncast(cluster *fdbtypes.FoundationDBCluster) (*MockAdminClient, error) {
	client := adminClientCache[cluster.Name]
	if client == nil {
		client = &MockAdminClient{Cluster: cluster}
		adminClientCache[cluster.Name] = client
	}
	return client, nil
}

// ClearMockAdminClients clears the cache of mock Admin clients
func ClearMockAdminClients() {
	adminClientCache = map[string]*MockAdminClient{}
}

// ConfigureDatabase changes the database configuration
func (client *MockAdminClient) ConfigureDatabase(configuration DatabaseConfiguration, newDatabase bool) error {
	client.DatabaseConfiguration = configuration
	return nil
}

// ExcludeInstances starts evacuating processes so that they can be removed
// from the database.
func (client *MockAdminClient) ExcludeInstances(addresses []string) error {
	client.ExcludedAddresses = append(client.ExcludedAddresses, addresses...)
	return nil
}

// IncludeInstances removes processes from the exclusion list and allows
// them to take on roles again.
func (client *MockAdminClient) IncludeInstances(addresses []string) error {
	newExclusions := make([]string, 0, len(client.ExcludedAddresses))
	for _, excludedAddress := range client.ExcludedAddresses {
		included := false
		for _, address := range addresses {
			if address == excludedAddress {
				included = true
				client.ReincludedAddresses = append(client.ReincludedAddresses, address)
				break
			}
		}
		if !included {
			newExclusions = append(newExclusions, excludedAddress)
		}
	}
	client.ExcludedAddresses = newExclusions
	return nil
}

// CanSafelyRemove checks whether it is safe to remove processes from the
// cluster
func (client *MockAdminClient) CanSafelyRemove(addresses []string) ([]string, error) {
	return nil, nil
}

// localityPolicy describes a policy for how data is replicated.
type localityPolicy interface {
	// BinaryRepresentation gets the encoded policy for use in database
	// configuration
	BinaryRepresentation() []byte
}

// singletonPolicy provides a policy that keeps a single replica of data
type singletonPolicy struct {
}

// BinaryRepresentation gets the encoded policy for use in database
// configuration
func (policy *singletonPolicy) BinaryRepresentation() []byte {
	return []byte("\x03\x00\x00\x00One")
}

// acrossPolicy provides a policy that replicates across fault domains
type acrossPolicy struct {
	Count     uint32
	Field     string
	Subpolicy localityPolicy
}

// BinaryRepresentation gets the encoded policy for use in database
// configuration
func (policy *acrossPolicy) BinaryRepresentation() []byte {
	intBuffer := [4]byte{}
	buffer := bytes.NewBuffer(nil)
	binary.LittleEndian.PutUint32(intBuffer[:], 6)
	buffer.Write(intBuffer[:])
	buffer.WriteString("Across")
	binary.LittleEndian.PutUint32(intBuffer[:], uint32(len(policy.Field)))
	buffer.Write(intBuffer[:])
	buffer.WriteString(policy.Field)
	binary.LittleEndian.PutUint32(intBuffer[:], policy.Count)
	buffer.Write(intBuffer[:])
	buffer.Write(policy.Subpolicy.BinaryRepresentation())
	return buffer.Bytes()
}
