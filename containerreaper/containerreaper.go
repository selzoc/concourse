package containerreaper

import (
	"errors"
	"time"

	"github.com/concourse/atc/db"
	"github.com/concourse/atc/worker"
	"github.com/pivotal-golang/lager"
)

type ContainerReaper interface {
	Run() error
}

//go:generate counterfeiter . ContainerReaperDB

type ContainerReaperDB interface {
	FindJobIDForBuild(buildID int) (int, bool, error)
	FindOrphanContainersWithInfiniteTTL() ([]db.SavedContainer, error)
	FindContainersFromSuccessfulBuildsWithInfiniteTTL() ([]db.SavedContainer, error)
	FindContainersFromUnsuccessfulBuildsWithInfiniteTTL() ([]db.SavedContainer, error)
	UpdateExpiresAtOnContainer(handle string, ttl time.Duration) error
}

type containerReaper struct {
	logger            lager.Logger
	workerClient      worker.Client
	db                ContainerReaperDB
	pipelineDBFactory db.PipelineDBFactory
}

func NewContainerReaper(
	logger lager.Logger,
	workerClient worker.Client,
	db ContainerReaperDB,
	pipelineDBFactory db.PipelineDBFactory,
) ContainerReaper {
	return &containerReaper{
		logger:            logger,
		workerClient:      workerClient,
		db:                db,
		pipelineDBFactory: pipelineDBFactory,
	}
}

func (cr *containerReaper) updateWorkerContainerTTL(handle string) error {
	workerContainer, found, err := cr.workerClient.LookupContainer(cr.logger, handle)
	if err != nil {
		cr.logger.Error("error-finding-worker-container", err)
		return err
	}

	if !found {
		cr.logger.Error("worker-containerr-not-found", nil)
		return errors.New("worker-container-not-found")
	}

	workerContainer.Release(worker.FinalTTL(worker.ContainerTTL))
	return nil
}

func (cr *containerReaper) release(handle string) error {
	err := cr.updateWorkerContainerTTL(handle)
	if err != nil {
		return err
	}

	err = cr.db.UpdateExpiresAtOnContainer(handle, worker.ContainerTTL)
	if err != nil {
		cr.logger.Error("error-updating-db-container-ttl", err)
	}
	return err
}

func (cr *containerReaper) Run() error {
	successfulContainers, err := cr.db.FindContainersFromSuccessfulBuildsWithInfiniteTTL()
	cr.logger.Error("running-container-reaper", nil)
	if err != nil {
		cr.logger.Error("failed-to-find-successful-containers", err)
	} else {
		for _, container := range successfulContainers {
			cr.logger.Error("successful-container: ", nil, lager.Data{"pipeline": container.PipelineID})
			cr.release(container.Handle)
		}
	}

	failedContainers, err := cr.db.FindContainersFromUnsuccessfulBuildsWithInfiniteTTL()
	if err != nil {
		cr.logger.Error("failed-to-find-unsuccessful-containers", err)
		return err
	}

	failedJobContainerMap := cr.buildFailedMap(failedContainers)
	successfulJobContainerMap := cr.buildSuccessMap(successfulContainers)

	for jobID, jobContainers := range failedJobContainerMap {
		maxFailedBuildID := -1
		for _, jobContainer := range jobContainers {
			if jobContainer.BuildID > maxFailedBuildID {
				maxFailedBuildID = jobContainer.BuildID
			}
		}

		for _, jobContainer := range jobContainers {
			maxSuccessfulBuildID := successfulJobContainerMap[jobID]

			if maxSuccessfulBuildID > maxFailedBuildID || maxFailedBuildID > jobContainer.BuildID {
				handle := jobContainer.Container.Handle
				cr.release(handle)
			}
		}
	}

	return nil
}

func (cr *containerReaper) buildSuccessMap(containers []db.SavedContainer) map[int]int {
	var jobContainerMap map[int]int
	jobContainerMap = make(map[int]int)

	if containers == nil {
		return jobContainerMap
	}

	for _, container := range containers {
		buildID := container.BuildID
		jobID, found, err := cr.db.FindJobIDForBuild(buildID)
		if err != nil || !found {
			cr.logger.Error("find-job-id-for-build", err, lager.Data{"build-id": buildID, "found": found})
			cr.release(container.Handle)
			continue
		}

		maxSuccessfulBuildID := jobContainerMap[jobID]
		if buildID > maxSuccessfulBuildID {
			jobContainerMap[jobID] = buildID
		}
	}

	return jobContainerMap
}

func (cr *containerReaper) buildFailedMap(containers []db.SavedContainer) map[int][]db.SavedContainer {
	var jobContainerMap map[int][]db.SavedContainer
	jobContainerMap = make(map[int][]db.SavedContainer)

	for _, container := range containers {
		pipelineDB, err := cr.pipelineDBFactory.BuildWithID(container.PipelineID)
		if err != nil {
			cr.logger.Error("no pipeline", err, lager.Data{"build-id": container.BuildID})
			cr.release(container.Handle)
			continue
		}

		pipelineConfig, _, found, err := pipelineDB.GetConfig()
		if err != nil || !found {
			cr.release(container.Handle)
			continue
		}

		jobExpired := true
		for _, jobConfig := range pipelineConfig.Jobs {
			if jobConfig.Name == container.JobName {
				jobExpired = false
				break
			}
		}

		if jobExpired {
			cr.release(container.Handle)
			continue
		}

		buildID := container.BuildID
		jobID, found, err := cr.db.FindJobIDForBuild(buildID)
		if err != nil || !found {
			cr.logger.Error("find-job-id-for-build", err, lager.Data{"build-id": buildID, "found": found})
			cr.release(container.Handle)
			continue
		}

		jobContainers := jobContainerMap[jobID]
		if jobContainers == nil {
			jobContainerMap[jobID] = []db.SavedContainer{container}
		} else {
			jobContainers = append(jobContainers, container)
			jobContainerMap[jobID] = jobContainers
		}
	}

	return jobContainerMap
}
