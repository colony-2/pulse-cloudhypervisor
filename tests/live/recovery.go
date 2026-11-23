package live

import (
	"encoding/hex"
	"strings"
)

// These nonsecret records are scoped to a single collector's private directory.
// The managed worker supplies the run and exact target before issuing a reboot.
type recoveryRun struct {
	Run, Candidate, Source, Mapping, Fixture, Worker, WorkerBoot string
}

type recoveryTarget struct {
	Run                                        recoveryRun
	Role, VM, ProviderBoot                     string
	DiskDevice, DiskInode, APIDevice, APIInode uint64
}

type recoveryObservation struct {
	Target                   recoveryTarget
	WorkerBoot, ProviderBoot string
}

func hexIdentity(value string, size int) bool {
	if len(value) != size || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func bootIdentity(value string) bool {
	return len(value) == 36 && value[8] == '-' && value[13] == '-' && value[18] == '-' && value[23] == '-' &&
		hexIdentity(strings.ReplaceAll(value, "-", ""), 32)
}

func checkRecovery(run recoveryRun, target recoveryTarget, observed recoveryObservation, workerBoot, fixture string) error {
	if !hexIdentity(run.Run, 32) || !hexIdentity(run.Candidate, 40) || !hexIdentity(run.Source, 64) ||
		!hexIdentity(run.Mapping, 64) || run.Fixture == "" || run.Fixture != fixture || run.Worker == "" ||
		!bootIdentity(workerBoot) || workerBoot != run.WorkerBoot || observed.WorkerBoot != workerBoot {
		return errFixture
	}
	if target.Run != run || target.Role != "provider" || !hexIdentity(target.VM, 32) || target.VM == run.Worker ||
		!bootIdentity(target.ProviderBoot) || target.ProviderBoot == workerBoot ||
		target.DiskDevice == 0 || target.DiskInode == 0 || target.APIDevice == 0 || target.APIInode == 0 ||
		observed.Target != target || !bootIdentity(observed.ProviderBoot) ||
		observed.ProviderBoot == target.ProviderBoot || observed.ProviderBoot == workerBoot {
		return errFixture
	}
	return nil
}
