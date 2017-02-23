package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"code.cloudfoundry.org/lager"
	"github.com/concourse/baggageclaim"
	"github.com/concourse/baggageclaim/uidgid"
	"github.com/concourse/baggageclaim/volume"
	"github.com/tedsuo/rata"
)

const httpUnprocessableEntity = 422

var ErrListVolumesFailed = errors.New("failed to list volumes")
var ErrGetVolumeFailed = errors.New("failed to get volume")
var ErrGetVolumeStatsFailed = errors.New("failed to get volume stats")
var ErrCreateVolumeFailed = errors.New("failed to create volume")
var ErrDestroyVolumeFailed = errors.New("failed to destroy volume")
var ErrSetPropertyFailed = errors.New("failed to set property on volume")
var ErrSetTTLFailed = errors.New("failed to set ttl on volume")
var ErrStreamInFailed = errors.New("failed to stream in to volume")
var ErrStreamOutFailed = errors.New("failed to stream out from volume")
var ErrStreamOutNotFound = errors.New("no such file or directory")

type VolumeServer struct {
	strategerizer volume.Strategerizer
	namespacer    uidgid.Namespacer
	volumeRepo    volume.Repository

	logger lager.Logger
}

func NewVolumeServer(
	logger lager.Logger,
	strategerizer volume.Strategerizer,
	namespacer uidgid.Namespacer,
	volumeRepo volume.Repository,
) *VolumeServer {
	return &VolumeServer{
		strategerizer: strategerizer,
		namespacer:    namespacer,
		volumeRepo:    volumeRepo,
		logger:        logger,
	}
}

func (vs *VolumeServer) CreateVolume(w http.ResponseWriter, req *http.Request) {
	hLog := vs.logger.Session("create-volume")

	hLog.Debug("start")
	defer hLog.Debug("done")

	var request baggageclaim.VolumeRequest
	err := json.NewDecoder(req.Body).Decode(&request)
	if err != nil {
		RespondWithError(w, ErrCreateVolumeFailed, http.StatusBadRequest)
		return
	}

	hLog = hLog.WithData(lager.Data{
		"handle":     request.Handle,
		"ttl":        request.TTLInSeconds,
		"privileged": request.Privileged,
		"strategy":   request.Strategy,
	})

	strategy, err := vs.strategerizer.StrategyFor(request)
	if err != nil {
		hLog.Error("could-not-produce-strategy", err)
		RespondWithError(w, ErrCreateVolumeFailed, httpUnprocessableEntity)
		return
	}

	hLog.Debug("creating")

	createdVolume, err := vs.volumeRepo.CreateVolume(
		request.Handle,
		strategy,
		volume.Properties(request.Properties),
		request.TTLInSeconds,
		request.Privileged,
	)

	if err != nil {
		hLog.Error("failed-to-create", err)

		var code int
		switch err {
		case volume.ErrParentVolumeNotFound:
			code = httpUnprocessableEntity
		case volume.ErrNoParentVolumeProvided:
			code = httpUnprocessableEntity
		default:
			code = http.StatusInternalServerError
		}
		RespondWithError(w, ErrCreateVolumeFailed, code)
		return
	}

	hLog = hLog.WithData(lager.Data{
		"volume": createdVolume.Handle,
	})

	hLog.Debug("created")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)

	if err := json.NewEncoder(w).Encode(createdVolume); err != nil {
		hLog.Error("failed-to-encode", err, lager.Data{
			"volume-path": createdVolume.Path,
		})
	}
}

func (vs *VolumeServer) DestroyVolume(w http.ResponseWriter, req *http.Request) {
	handle := rata.Param(req, "handle")

	hLog := vs.logger.Session("destroy", lager.Data{
		"volume": handle,
	})

	hLog.Debug("start")
	defer hLog.Debug("done")

	err := vs.volumeRepo.DestroyVolume(handle)
	if err != nil {
		if err == volume.ErrVolumeDoesNotExist {
			hLog.Info("volume-does-not-exist")
			RespondWithError(w, ErrSetTTLFailed, http.StatusNotFound)
		} else {
			hLog.Error("failed-to-destroy", err)
			RespondWithError(w, ErrSetTTLFailed, http.StatusInternalServerError)
		}

		return
	}

	hLog.Info("destroyed")

	w.WriteHeader(http.StatusNoContent)
}

func (vs *VolumeServer) ListVolumes(w http.ResponseWriter, req *http.Request) {
	hLog := vs.logger.Session("list-volumes")

	hLog.Debug("start")
	defer hLog.Debug("done")

	w.Header().Set("Content-Type", "application/json")

	properties, err := ConvertQueryToProperties(req.URL.Query())
	if err != nil {
		RespondWithError(w, err, httpUnprocessableEntity)
		return
	}

	volumes, _, err := vs.volumeRepo.ListVolumes(properties)
	if err != nil {
		hLog.Error("failed-to-list-volumes", err)
		RespondWithError(w, ErrListVolumesFailed, http.StatusInternalServerError)
		return
	}

	if err := json.NewEncoder(w).Encode(volumes); err != nil {
		hLog.Error("failed-to-encode", err)
	}
}

func (vs *VolumeServer) GetVolume(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	handle := rata.Param(req, "handle")

	hLog := vs.logger.Session("get-volume", lager.Data{
		"volume": handle,
	})

	hLog.Debug("start")
	defer hLog.Debug("done")

	vol, found, err := vs.volumeRepo.GetVolume(handle)
	if err != nil {
		hLog.Error("failed-to-get-volume", err)
		RespondWithError(w, ErrGetVolumeFailed, http.StatusInternalServerError)
		return
	}

	if !found {
		hLog.Info("volume-not-found")
		RespondWithError(w, ErrGetVolumeFailed, http.StatusNotFound)
		return
	}

	if err := json.NewEncoder(w).Encode(vol); err != nil {
		hLog.Error("failed-to-encode", err)
	}
}

func (vs *VolumeServer) GetVolumeStats(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	handle := rata.Param(req, "handle")

	hLog := vs.logger.Session("get-volume-stats", lager.Data{
		"volume": handle,
	})

	hLog.Debug("start")
	defer hLog.Debug("done")

	vol, found, err := vs.volumeRepo.GetVolumeStats(handle)
	if err != nil {
		hLog.Error("failed-to-get-volume-stats", err)
		RespondWithError(w, ErrGetVolumeStatsFailed, http.StatusInternalServerError)
		return
	}

	if !found {
		RespondWithError(w, ErrGetVolumeStatsFailed, http.StatusNotFound)
		return
	}

	if err := json.NewEncoder(w).Encode(vol); err != nil {
		hLog.Error("failed-to-encode", err)
	}
}

func (vs *VolumeServer) SetProperty(w http.ResponseWriter, req *http.Request) {
	handle := rata.Param(req, "handle")
	propertyName := rata.Param(req, "property")

	hLog := vs.logger.Session("set-property", lager.Data{
		"volume":   handle,
		"property": propertyName,
	})

	hLog.Debug("start")
	defer hLog.Debug("done")

	var request baggageclaim.PropertyRequest
	err := json.NewDecoder(req.Body).Decode(&request)
	if err != nil {
		RespondWithError(w, ErrSetPropertyFailed, http.StatusBadRequest)
		return
	}

	propertyValue := request.Value

	hLog.Debug("setting-property")

	err = vs.volumeRepo.SetProperty(handle, propertyName, propertyValue)
	if err != nil {
		hLog.Error("failed-to-set-property", err)

		if err == volume.ErrVolumeDoesNotExist {
			RespondWithError(w, ErrSetPropertyFailed, http.StatusNotFound)
		} else {
			RespondWithError(w, ErrSetPropertyFailed, http.StatusInternalServerError)
		}

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (vs *VolumeServer) SetTTL(w http.ResponseWriter, req *http.Request) {
	handle := rata.Param(req, "handle")

	hLog := vs.logger.Session("set-ttl", lager.Data{
		"volume": handle,
	})

	hLog.Debug("start")
	defer hLog.Debug("done")

	var request baggageclaim.TTLRequest
	err := json.NewDecoder(req.Body).Decode(&request)
	if err != nil {
		RespondWithError(w, ErrSetTTLFailed, http.StatusBadRequest)
		return
	}

	ttl := request.Value

	hLog.Debug("setting-ttl", lager.Data{"ttl": ttl})

	err = vs.volumeRepo.SetTTL(handle, ttl)
	if err != nil {
		hLog.Error("failed-to-set-ttl", err)

		if err == volume.ErrVolumeDoesNotExist {
			RespondWithError(w, ErrSetTTLFailed, http.StatusNotFound)
		} else {
			RespondWithError(w, ErrSetTTLFailed, http.StatusInternalServerError)
		}

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (vs *VolumeServer) StreamIn(w http.ResponseWriter, req *http.Request) {
	handle := rata.Param(req, "handle")

	hLog := vs.logger.Session("stream-in", lager.Data{
		"volume": handle,
	})

	hLog.Debug("start")
	defer hLog.Debug("done")

	vol, found, err := vs.volumeRepo.GetVolume(handle)
	if err != nil {
		hLog.Error("failed-to-get-volume", err)
		RespondWithError(w, ErrStreamInFailed, http.StatusInternalServerError)
		return
	}

	if !found {
		hLog.Info("volume-not-found")
		RespondWithError(w, ErrStreamInFailed, http.StatusNotFound)
		return
	}

	var subPath string
	if queryPath, ok := req.URL.Query()["path"]; ok {
		subPath = queryPath[0]
	}

	destinationPath := filepath.Join(vol.Path, subPath)

	hLog = hLog.WithData(lager.Data{
		"sub-path":  subPath,
		"full-path": destinationPath,
	})

	err = os.MkdirAll(destinationPath, 0755)
	if err != nil {
		hLog.Error("failed-to-create-destination-path", err)
		RespondWithError(w, ErrStreamInFailed, http.StatusInternalServerError)
		return
	}

	if !vol.Privileged {
		err := vs.namespacer.NamespacePath(hLog, vol.Path)
		if err != nil {
			hLog.Error("failed-to-namespace-path", err)
			RespondWithError(w, ErrStreamInFailed, http.StatusInternalServerError)
			return
		}
	}

	tarCommand := exec.Command("tar", "-x", "-C", destinationPath)
	tarCommand.Stdin = req.Body

	if !vol.Privileged {
		vs.namespacer.NamespaceCommand(tarCommand)
	}

	err = tarCommand.Run()
	if err != nil {
		hLog.Error("failed-to-stream-into-volume", err)

		if _, ok := err.(*exec.ExitError); ok {
			RespondWithError(w, ErrStreamInFailed, http.StatusBadRequest)
			return
		}

		RespondWithError(w, ErrStreamInFailed, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (vs *VolumeServer) StreamOut(w http.ResponseWriter, req *http.Request) {
	handle := rata.Param(req, "handle")

	hLog := vs.logger.Session("stream-out", lager.Data{
		"volume": handle,
	})

	hLog.Debug("start")
	defer hLog.Debug("done")

	vol, found, err := vs.volumeRepo.GetVolume(handle)
	if err != nil {
		hLog.Error("failed-to-get-volume", err)
		RespondWithError(w, ErrStreamOutFailed, http.StatusInternalServerError)
		return
	}

	if !found {
		hLog.Error("volume-not-found", err)
		RespondWithError(w, ErrStreamOutFailed, http.StatusNotFound)
		return
	}

	var subPath string
	if queryPath, ok := req.URL.Query()["path"]; ok {
		subPath = queryPath[0]
	}

	srcPath := filepath.Join(vol.Path, subPath)

	hLog = hLog.WithData(lager.Data{
		"sub-path":  subPath,
		"full-path": srcPath,
	})

	err = vs.streamOutSrc(vol, srcPath, w)
	if err != nil {
		if os.IsNotExist(err) {
			hLog.Info("source-path-not-found")
			RespondWithError(w, ErrStreamOutNotFound, http.StatusNotFound)
			return
		}

		hLog.Error("failed-to-stream-out-from-volume", err)

		if _, ok := err.(*exec.ExitError); ok {
			RespondWithError(w, ErrStreamOutFailed, http.StatusBadRequest)
			return
		}

		RespondWithError(w, ErrStreamOutFailed, http.StatusInternalServerError)
		return
	}
}

func (vs *VolumeServer) streamOutSrc(vol volume.Volume, srcPath string, w http.ResponseWriter) error {
	fileInfo, err := os.Stat(srcPath)
	if err != nil {
		return err
	}

	var tarCommandPath, tarCommandDir string

	if fileInfo.IsDir() {
		tarCommandPath = "."
		tarCommandDir = srcPath
	} else {
		tarCommandPath = filepath.Base(srcPath)
		tarCommandDir = filepath.Dir(srcPath)
	}

	tarCommand := exec.Command("tar", "-c", tarCommandPath)
	tarCommand.Dir = tarCommandDir

	if !vol.Privileged {
		vs.namespacer.NamespaceCommand(tarCommand)
	}

	readCloser, err := tarCommand.StdoutPipe()
	if err != nil {
		return err
	}

	err = tarCommand.Start()
	if err != nil {
		return err
	}

	_, err = io.Copy(w, readCloser)
	if err != nil {
		return err
	}

	return tarCommand.Wait()
}
