package scbe

import (
	"errors"
	"fmt"
	"github.com/IBM/ubiquity/logutil"
	"github.com/IBM/ubiquity/resources"
	"github.com/jinzhu/gorm"
	"strconv"
	"sync"
	"encoding/json"
)

type scbeLocalClient struct {
	logger         logutil.Logger
	dataModel      ScbeDataModel
	scbeRestClient ScbeRestClient
	isActivated    bool
	config         resources.ScbeConfig
	activationLock *sync.RWMutex
}

const (
	OptionNameForServiceName = "profile"
	OptionNameForVolumeSize  = "size"
	volumeNamePrefix         = "u_"
	AttachedToNothing        = "" // during provisioning the volume is not attached to any host
	EmptyHost                = ""
	ComposeVolumeName    = volumeNamePrefix + "%s_%s" // e.g u_instance1_volName
	MaxVolumeNameLength  = 63                         // IBM block storage max volume name cannot exceed this length
)

func NewScbeLocalClient(config resources.ScbeConfig, database *gorm.DB) (resources.StorageClient, error) {
	logger := logutil.GetLogger()
	datamodel := NewScbeDataModel(database, resources.SCBE)
	err := datamodel.CreateVolumeTable()
	if err != nil {
		return &scbeLocalClient{}, logger.ErrorRet(err, "failed")
	}
	scbeRestClient := NewScbeRestClient(config.ConnectionInfo)
	return NewScbeLocalClientWithNewScbeRestClientAndDataModel(config, datamodel, scbeRestClient)
}
func NewScbeLocalClientWithNewScbeRestClientAndDataModel(config resources.ScbeConfig, dataModel ScbeDataModel, scbeRestClient ScbeRestClient) (resources.StorageClient, error) {
	client := &scbeLocalClient{
		logger:         logutil.GetLogger(),
		scbeRestClient: scbeRestClient, // TODO need to mock it in more advance way
		dataModel:      dataModel,
		config:         config,
		activationLock: &sync.RWMutex{},
	}
	if err := basicScbeLocalClientStartupAndValidation(client, config); err != nil {
		return &scbeLocalClient{}, err
	}
	return client, nil
}

// basicScbeLocalClientStartup validate config params, login to SCBE and validate default exist
func basicScbeLocalClientStartupAndValidation(s *scbeLocalClient, config resources.ScbeConfig) error {
	if err := validateScbeConfig(config); err != nil {
		return err
	}

	if err := s.scbeRestClient.Login(); err != nil {
		return s.logger.ErrorRet(err, "scbeRestClient.Login() failed")
	}
	s.logger.Info("scbeRestClient.Login() succeeded", logutil.Args{{"SCBE", s.config.ConnectionInfo.ManagementIP}})

	isExist, err := s.scbeRestClient.ServiceExist(s.config.DefaultService)
	if err != nil {
		return s.logger.ErrorRet(err, "scbeRestClient.ServiceExist failed")
	}

	if isExist == false {
		return s.logger.ErrorRet(&activateDefaultServiceError{s.config.DefaultService, s.config.ConnectionInfo.ManagementIP}, "failed")
	}
	s.logger.Info("The default service exist in SCBE", logutil.Args{{s.config.ConnectionInfo.ManagementIP, s.config.DefaultService}})
	return nil
}

func validateScbeConfig(config resources.ScbeConfig) error {
	logger := logutil.GetLogger()

	if config.DefaultVolumeSize == "" {
		// means customer didn't configure the default
		config.DefaultVolumeSize = resources.DefaultForScbeConfigParamDefaultVolumeSize
		logger.Debug("No DefaultVolumeSize defined in conf file, so set the DefaultVolumeSize to value " + resources.DefaultForScbeConfigParamDefaultVolumeSize)
	}
	_, err := strconv.Atoi(config.DefaultVolumeSize)
	if err != nil {
		return logger.ErrorRet(&ConfigDefaultSizeNotNumError{}, "failed")
	}

	if len(config.UbiquityInstanceName) > resources.UbiquityInstanceNameMaxSize {
		return logger.ErrorRet(&ConfigScbeUbiquityInstanceNameWrongSize{}, "failed")
	}

	// TODO add more verification on the config file.
	return nil
}

func (s *scbeLocalClient) Activate(activateRequest resources.ActivateRequest) error {
	defer s.logger.Trace(logutil.DEBUG)()

	s.activationLock.RLock()
	if s.isActivated {
		s.activationLock.RUnlock()
		return nil
	}
	s.activationLock.RUnlock()

	s.activationLock.Lock() //get a write lock to prevent others from repeating these actions
	defer s.activationLock.Unlock()

	// Nothing special to activate SCBE
	s.isActivated = true
	return nil
}

// CreateVolume parse and validate the given options and trigger the volume creation
func (s *scbeLocalClient) CreateVolume(createVolumeRequest resources.CreateVolumeRequest) (err error) {
	defer s.logger.Trace(logutil.DEBUG)()

	_, volExists, err := s.dataModel.GetVolume(createVolumeRequest.Name)
	if err != nil {
		return s.logger.ErrorRet(err, "dataModel.GetVolume failed", logutil.Args{{"name", createVolumeRequest.Name}})
	}

	// validate volume doesn't exist
	if volExists {
		return s.logger.ErrorRet(&volAlreadyExistsError{createVolumeRequest.Name}, "failed")
	}

	// validate size option given
	sizeStr, ok := createVolumeRequest.Opts[OptionNameForVolumeSize]
	if !ok {
		sizeStr = s.config.DefaultVolumeSize
		s.logger.Debug("No size given to create volume, so using the default_size",
			logutil.Args{{"volume", createVolumeRequest.Name}, {"default_size", sizeStr}})
	}

	// validate size is a number
	size, err := strconv.Atoi(sizeStr.(string))
	if err != nil {
		return s.logger.ErrorRet(&provisionParamIsNotNumberError{createVolumeRequest.Name, OptionNameForVolumeSize}, "failed")
	}
	// Get the profile option
	profile := s.config.DefaultService
	if createVolumeRequest.Opts[OptionNameForServiceName] != "" && createVolumeRequest.Opts[OptionNameForServiceName] != nil {
		profile = createVolumeRequest.Opts[OptionNameForServiceName].(string)
	}

	// Generate the designated volume name by template
	volNameToCreate := fmt.Sprintf(ComposeVolumeName, s.config.UbiquityInstanceName, createVolumeRequest.Name)

	// Validate volume length ok
	volNamePrefixForCheckLength := fmt.Sprintf(ComposeVolumeName, s.config.UbiquityInstanceName, "")
	volNamePrefixForCheckLengthLen := len(volNamePrefixForCheckLength)
	if len(volNameToCreate) > MaxVolumeNameLength {
		maxVolLength := MaxVolumeNameLength - volNamePrefixForCheckLengthLen // its dynamic because it depends on the UbiquityInstanceName len
		return s.logger.ErrorRet(&VolumeNameExceededMaxLengthError{createVolumeRequest.Name, maxVolLength}, "failed")
	}
	// Provision the volume on SCBE service
	volInfo := ScbeVolumeInfo{}
	volInfo, err = s.scbeRestClient.CreateVolume(volNameToCreate, profile, size)
	if err != nil {
		return s.logger.ErrorRet(err, "scbeRestClient.CreateVolume failed")
	}

	err = s.dataModel.InsertVolume(createVolumeRequest.Name, volInfo.Wwn, AttachedToNothing)
	if err != nil {
		return s.logger.ErrorRet(err, "dataModel.InsertVolume failed")
	}

	s.logger.Info("succeeded", logutil.Args{{"volume", createVolumeRequest.Name}, {"profile", profile}})
	return nil
}

func (s *scbeLocalClient) RemoveVolume(removeVolumeRequest resources.RemoveVolumeRequest) (err error) {
	defer s.logger.Trace(logutil.DEBUG)()

	existingVolume, volExists, err := s.dataModel.GetVolume(removeVolumeRequest.Name)
	if err != nil {
		return s.logger.ErrorRet(err, "dataModel.GetVolume failed")
	}

	if volExists == false {
		return s.logger.ErrorRet(fmt.Errorf("Volume [%s] not found", removeVolumeRequest.Name), "failed")
	}

	if err = s.dataModel.DeleteVolume(removeVolumeRequest.Name); err != nil {
		return s.logger.ErrorRet(err, "dataModel.DeleteVolume failed")
	}

	if err = s.scbeRestClient.DeleteVolume(existingVolume.WWN); err != nil {
		return s.logger.ErrorRet(err, "scbeRestClient.DeleteVolume failed")
	}

	return nil
}

func (s *scbeLocalClient) GetVolume(getVolumeRequest resources.GetVolumeRequest) (resources.Volume, error) {
	defer s.logger.Trace(logutil.DEBUG)()

	existingVolume, volExists, err := s.dataModel.GetVolume(getVolumeRequest.Name)
	if err != nil {
		return resources.Volume{}, s.logger.ErrorRet(err, "dataModel.GetVolume failed")
	}
	if volExists == false {
		return resources.Volume{}, s.logger.ErrorRet(errors.New("Volume not found"), "failed")
	}

	return resources.Volume{Name: existingVolume.Volume.Name, Backend: existingVolume.Volume.Backend}, nil
}

func (s *scbeLocalClient) GetVolumeConfig(getVolumeConfigRequest resources.GetVolumeConfigRequest) (map[string]interface{}, error) {
	defer s.logger.Trace(logutil.DEBUG)()

	// get volume wwn from name
	scbeVolume, volExists, err := s.dataModel.GetVolume(getVolumeConfigRequest.Name)
	if err != nil {
		return nil, s.logger.ErrorRet(err, "dataModel.GetVolume failed")
	}

	// verify volume exists
	if !volExists {
		return nil, s.logger.ErrorRet(errors.New("Volume not found"), "failed")
	}

	// get volume full info from scbe
	volumeInfo, err := s.scbeRestClient.GetVolumes(scbeVolume.WWN)
	if err != nil {
		return nil, s.logger.ErrorRet(err, "scbeRestClient.GetVolumes failed")
	}

	// verify volume is found
	if len(volumeInfo) != 1 {
		return nil, s.logger.ErrorRet(&volumeNotFoundError{getVolumeConfigRequest.Name}, "failed", logutil.Args{{"volumeInfo", volumeInfo}})
	}

	// serialize scbeVolumeInfo to json
	jsonData, err := json.Marshal(volumeInfo[0])
	if err != nil {
		return nil, s.logger.ErrorRet(err, "json.Marshal failed")
	}

	// convert json to map[string]interface{}
	var volConfig map[string]interface{}
	if err = json.Unmarshal(jsonData, &volConfig); err != nil {
		return nil, s.logger.ErrorRet(err, "json.Unmarshal failed")
	}

	return volConfig, nil
}

func (s *scbeLocalClient) Attach(attachRequest resources.AttachRequest) (string, error) {
	defer s.logger.Trace(logutil.DEBUG)()

	if attachRequest.Host == EmptyHost {
		return "", s.logger.ErrorRet(
			&InValidRequestError{"attachRequest", "Host", attachRequest.Host, "none empty string"}, "failed")
	}
	if attachRequest.Name == "" {
		return "", s.logger.ErrorRet(
			&InValidRequestError{"attachRequest", "Name", attachRequest.Name, "none empty string"}, "failed")
	}

	host2attach := attachRequest.Host

	existingVolume, volExists, err := s.dataModel.GetVolume(attachRequest.Name)
	if err != nil {
		return "", s.logger.ErrorRet(err, "dataModel.GetVolume failed")
	}

	if !volExists {
		return "", s.logger.ErrorRet(&volumeNotFoundError{attachRequest.Name}, "failed")
	}

	if existingVolume.AttachTo == host2attach {
		// if already map to the given host then just ignore and succeed to attach
		s.logger.Info("Volume already attached, skip backend attach", logutil.Args{{"volume", attachRequest.Name}, {"host", host2attach}})
		volumeMountpoint := fmt.Sprintf(resources.PathToMountUbiquityBlockDevices, existingVolume.WWN)
		return volumeMountpoint, nil
	} else if existingVolume.AttachTo != "" {
		return "", s.logger.ErrorRet(&volAlreadyAttachedError{attachRequest.Name, existingVolume.AttachTo}, "failed")
	}

	s.logger.Debug("Attaching", logutil.Args{{"volume", existingVolume}})
	if _, err = s.scbeRestClient.MapVolume(existingVolume.WWN, host2attach); err != nil {
		return "", s.logger.ErrorRet(err, "scbeRestClient.MapVolume failed")
	}

	if err = s.dataModel.UpdateVolumeAttachTo(attachRequest.Name, existingVolume, host2attach); err != nil {
		return "", s.logger.ErrorRet(err, "dataModel.UpdateVolumeAttachTo failed")
	}

	volumeMountpoint := fmt.Sprintf(resources.PathToMountUbiquityBlockDevices, existingVolume.WWN)
	return volumeMountpoint, nil
}

func (s *scbeLocalClient) Detach(detachRequest resources.DetachRequest) (err error) {
	defer s.logger.Trace(logutil.DEBUG)()
	host2detach := detachRequest.Host

	existingVolume, volExists, err := s.dataModel.GetVolume(detachRequest.Name)
	if err != nil {
		return s.logger.ErrorRet(err, "dataModel.GetVolume failed")
	}

	if !volExists {
		return s.logger.ErrorRet(&volumeNotFoundError{detachRequest.Name}, "failed")
	}

	// Fail if vol already detach
	if existingVolume.AttachTo == EmptyHost {
		return s.logger.ErrorRet(&volNotAttachedError{detachRequest.Name}, "failed")
	}

	s.logger.Debug("Detaching", logutil.Args{{"volume", existingVolume}})
	if err = s.scbeRestClient.UnmapVolume(existingVolume.WWN, host2detach); err != nil {
		return s.logger.ErrorRet(err, "scbeRestClient.UnmapVolume failed")
	}

	if err = s.dataModel.UpdateVolumeAttachTo(detachRequest.Name, existingVolume, EmptyHost); err != nil {
		return s.logger.ErrorRet(err, "dataModel.UpdateVolumeAttachTo failed")
	}

	return nil
}

func (s *scbeLocalClient) ListVolumes(listVolumesRequest resources.ListVolumesRequest) ([]resources.Volume, error) {
	defer s.logger.Trace(logutil.DEBUG)()
	var err error

	volumesInDb, err := s.dataModel.ListVolumes()
	if err != nil {
		return nil, s.logger.ErrorRet(err, "dataModel.ListVolumes failed")
	}

	s.logger.Debug("Volumes in db", logutil.Args{{"num", len(volumesInDb)}})
	var volumes []resources.Volume
	for _, volume := range volumesInDb {
		s.logger.Debug("Volumes from db", logutil.Args{{"volume", volume}})
		volumeMountpoint, err := s.getVolumeMountPoint(volume)
		if err != nil {
			return nil, s.logger.ErrorRet(err, "getVolumeMountPoint failed")
		}

		volumes = append(volumes, resources.Volume{Name: volume.Volume.Name, Mountpoint: volumeMountpoint})
	}

	return volumes, nil
}

func (s *scbeLocalClient) createVolume(name string, wwn string, profile string) error {
	defer s.logger.Trace(logutil.DEBUG)()

	if err := s.dataModel.InsertVolume(name, wwn, ""); err != nil {
		return s.logger.ErrorRet(err, "dataModel.InsertVolume failed")
	}

	return nil
}
func (s *scbeLocalClient) getVolumeMountPoint(volume ScbeVolume) (string, error) {
	defer s.logger.Trace(logutil.DEBUG)()

	//TODO return mountpoint
	return "some mount point", nil
}
