package streaming

import (
	"fmt"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/cosmos/cosmos-sdk/store/streaming/file"
	"github.com/cosmos/cosmos-sdk/store/types"
	"github.com/tendermint/tendermint/libs/log"

	"github.com/spf13/cast"
)

type (
	// AppOptions is an interface for accessing application options
	AppOptions interface {
		Get(string) interface{}
	}
	// ServiceConstructor is used to construct a streaming service
	ServiceConstructor func(AppOptions, []types.StoreKey, types.Codec, log.Logger, string) (types.StreamingService, error)
)

// ServiceType enum for specifying the type of StreamingService
type ServiceType int

const (
	Unknown ServiceType = iota
	File
)

// Streaming option keys
const (
	OptStreamersFilePrefix          = "streamers.file.prefix"
	OptStreamersFileWriteDir        = "streamers.file.write_dir"
	OptStreamersFileOutputMetadata  = "streamers.file.output-metadata"
	OptStreamersFileStopNodeOnError = "streamers.file.stop-node-on-error"
	OptStreamersFileFsync           = "streamers.file.fsync"

	OptStoreStreamers = "store.streamers"
)

// ServiceTypeFromString returns the streaming.ServiceType corresponding to the
// provided name.
func ServiceTypeFromString(name string) ServiceType {
	switch strings.ToLower(name) {
	case "file", "f":
		return File

	default:
		return Unknown
	}
}

// String returns the string name of a streaming.ServiceType
func (sst ServiceType) String() string {
	switch sst {
	case File:
		return "file"

	default:
		return "unknown"
	}
}

// ServiceConstructorLookupTable is a mapping of streaming.ServiceTypes to
// streaming.ServiceConstructors types.
var ServiceConstructorLookupTable = map[ServiceType]ServiceConstructor{
	File: NewFileStreamingService,
}

// NewServiceConstructor returns the streaming.ServiceConstructor corresponding
// to the provided name.
func NewServiceConstructor(name string) (ServiceConstructor, error) {
	ssType := ServiceTypeFromString(name)
	if ssType == Unknown {
		return nil, fmt.Errorf("unrecognized streaming service name %s", name)
	}

	if constructor, ok := ServiceConstructorLookupTable[ssType]; ok && constructor != nil {
		return constructor, nil
	}

	return nil, fmt.Errorf("streaming service constructor of type %s not found", ssType.String())
}

// NewFileStreamingService is the streaming.ServiceConstructor function for
// creating a FileStreamingService.
func NewFileStreamingService(
	opts AppOptions,
	keys []types.StoreKey,
	marshaller types.Codec,
	logger log.Logger,
	homePath string,
) (types.StreamingService, error) {
	filePrefix := cast.ToString(opts.Get(OptStreamersFilePrefix))
	fileDir := cast.ToString(opts.Get(OptStreamersFileWriteDir))
	outputMetadata := cast.ToBool(opts.Get(OptStreamersFileOutputMetadata))
	stopNodeOnErr := cast.ToBool(opts.Get(OptStreamersFileStopNodeOnError))
	fsync := cast.ToBool(opts.Get(OptStreamersFileFsync))

	// relative path is based on node home directory.
	if !path.IsAbs(fileDir) {
		fileDir = path.Join(homePath, fileDir)
	}

	// try to create output directory if not exists.
	if _, err := os.Stat(fileDir); os.IsNotExist(err) {
		if err = os.MkdirAll(fileDir, os.ModePerm); err != nil {
			return nil, err
		}
	}

	return file.NewStreamingService(fileDir, filePrefix, keys, marshaller, logger, outputMetadata, stopNodeOnErr, fsync)
}

// LoadStreamingServices is a function for loading StreamingServices onto the
// BaseApp using the provided AppOptions, codec, and keys. It returns the
// WaitGroup and quit channel used to synchronize with the streaming services
// and any error that occurs during the setup.
func LoadStreamingServices(
	appOpts AppOptions,
	appCodec types.Codec,
	logger log.Logger,
	keys map[string]*types.KVStoreKey,
	homePath string,
) ([]types.StreamingService, *sync.WaitGroup, error) {
	// waitgroup and quit channel for optional shutdown coordination of the streaming service(s)
	wg := new(sync.WaitGroup)

	// configure state listening capabilities using AppOptions
	streamers := cast.ToStringSlice(appOpts.Get(OptStoreStreamers))
	activeStreamers := make([]types.StreamingService, 0, len(streamers))

	for _, streamerName := range streamers {
		var exposeStoreKeys []types.StoreKey

		// get the store keys allowed to be exposed for this streaming service
		exposeKeyStrs := cast.ToStringSlice(appOpts.Get(fmt.Sprintf("streamers.%s.keys", streamerName)))

		// if list contains '*', expose all store keys
		if types.SliceContains(exposeKeyStrs, "*") {
			exposeStoreKeys = make([]types.StoreKey, 0, len(keys))
			for _, storeKey := range keys {
				exposeStoreKeys = append(exposeStoreKeys, storeKey)
			}
		} else {
			exposeStoreKeys = make([]types.StoreKey, 0, len(exposeKeyStrs))
			for _, keyStr := range exposeKeyStrs {
				if storeKey, ok := keys[keyStr]; ok {
					exposeStoreKeys = append(exposeStoreKeys, storeKey)
				}
			}
		}

		if len(exposeStoreKeys) == 0 {
			continue
		}

		constructor, err := NewServiceConstructor(streamerName)
		if err != nil {
			// Close any services we may have already spun up before hitting the error
			// on this one.
			for _, activeStreamer := range activeStreamers {
				activeStreamer.Close()
			}

			return nil, nil, err
		}

		// Generate the streaming service using the constructor, appOptions, and the
		// StoreKeys we want to expose.
		streamingService, err := constructor(appOpts, exposeStoreKeys, appCodec, logger, homePath)
		if err != nil {
			// Close any services we may have already spun up before hitting the error
			// on this one.
			for _, activeStreamer := range activeStreamers {
				activeStreamer.Close()
			}

			return nil, nil, err
		}

		// kick off the background streaming service loop
		streamingService.Stream(wg)

		// add to the list of active streamers
		activeStreamers = append(activeStreamers, streamingService)
	}

	// If there are no active streamers, activeStreamers is empty (len == 0) and
	// the waitGroup is not waiting on anything.
	return activeStreamers, wg, nil
}
