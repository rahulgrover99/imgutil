package acceptance

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"testing"
	"time"

	ggcrauthn "github.com/google/go-containerregistry/pkg/authn"

	dockertypes "github.com/docker/docker/api/types"
	dockerclient "github.com/docker/docker/client"
	ggcrname "github.com/google/go-containerregistry/pkg/name"
	ggcrremote "github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpacks/imgutil"
	"github.com/buildpacks/imgutil/local"
	"github.com/buildpacks/imgutil/remote"
	h "github.com/buildpacks/imgutil/testhelpers"
)

var localTestRegistry *h.DockerRegistry

func newTestImageName(suffixOpt ...string) string {
	suffix := ""
	if len(suffixOpt) == 1 {
		suffix = suffixOpt[0]
	}
	return fmt.Sprintf("%s:%s/imgutil-acceptance-%s%s", localTestRegistry.Host, localTestRegistry.Port, h.RandString(10), suffix)
}

func TestAcceptance(t *testing.T) {
	rand.Seed(time.Now().UTC().UnixNano())

	localTestRegistry = h.NewDockerRegistry()
	localTestRegistry.Start(t)
	defer localTestRegistry.Stop(t)

	spec.Run(t, "Reproducibility", testReproducibility, spec.Sequential(), spec.Report(report.Terminal{}))
}

func testReproducibility(t *testing.T, when spec.G, it spec.S) {
	var (
		imageName1, imageName2 string
		mutateAndSave          func(t *testing.T, image imgutil.Image)
		dockerClient           dockerclient.CommonAPIClient
		daemonInfo             dockertypes.Info
		runnableBaseImageName  string
	)

	it.Before(func() {
		var err error

		dockerClient = h.DockerCli(t)

		daemonInfo, err = dockerClient.Info(context.TODO())
		h.AssertNil(t, err)

		runnableBaseImageName = "busybox@sha256:915f390a8912e16d4beb8689720a17348f3f6d1a7b659697df850ab625ea29d5"
		if daemonInfo.OSType == "windows" {
			runnableBaseImageName = "mcr.microsoft.com/windows/nanoserver@sha256:06281772b6a561411d4b338820d94ab1028fdeb076c85350bbc01e80c4bfa2b4"
		}
		h.PullImage(dockerClient, runnableBaseImageName)

		imageName1 = newTestImageName()
		imageName2 = newTestImageName()
		labelKey := "label-key-" + h.RandString(10)
		labelVal := "label-val-" + h.RandString(10)
		envKey := "env-key-" + h.RandString(10)
		envVal := "env-val-" + h.RandString(10)
		workingDir := "working-dir-" + h.RandString(10)
		layer1 := randomLayer(t, daemonInfo.OSType)
		layer2 := randomLayer(t, daemonInfo.OSType)

		mutateAndSave = func(t *testing.T, img imgutil.Image) {
			h.AssertNil(t, img.AddLayer(layer1))
			h.AssertNil(t, img.AddLayer(layer2))
			h.AssertNil(t, img.SetLabel(labelKey, labelVal))
			h.AssertNil(t, img.SetEnv(envKey, envVal))
			h.AssertNil(t, img.SetEntrypoint("some", "entrypoint"))
			h.AssertNil(t, img.SetCmd("some", "cmd"))
			h.AssertNil(t, img.SetWorkingDir(workingDir))
			h.AssertNil(t, img.Save())
		}
	})

	it.After(func() {
		// clean up any local images
		h.DockerRmi(dockerClient, imageName1)
		h.DockerRmi(dockerClient, imageName2)
	})

	it("remote/remote", func() {
		img1, err := remote.NewImage(imageName1, localTestRegistry.GGCRKeychain(), remote.FromBaseImage(runnableBaseImageName))
		h.AssertNil(t, err)
		mutateAndSave(t, img1)

		img2, err := remote.NewImage(imageName2, localTestRegistry.GGCRKeychain(), remote.FromBaseImage(runnableBaseImageName))
		h.AssertNil(t, err)
		mutateAndSave(t, img2)

		compare(t, imageName1, imageName2, localTestRegistry.GGCRKeychain())
	})

	it("local/local", func() {
		img1, err := local.NewImage(imageName1, dockerClient, local.FromBaseImage(runnableBaseImageName))
		h.AssertNil(t, err)
		mutateAndSave(t, img1)
		h.AssertNil(t, h.PushImage(dockerClient, imageName1, localTestRegistry.DockerRegistryAuth()))

		img2, err := local.NewImage(imageName2, dockerClient, local.FromBaseImage(runnableBaseImageName))
		h.AssertNil(t, err)
		mutateAndSave(t, img2)
		h.AssertNil(t, h.PushImage(dockerClient, imageName2, localTestRegistry.DockerRegistryAuth()))

		compare(t, imageName1, imageName2, localTestRegistry.GGCRKeychain())
	})

	it("remote/local", func() {
		img1, err := remote.NewImage(imageName1, localTestRegistry.GGCRKeychain(), remote.FromBaseImage(runnableBaseImageName))
		h.AssertNil(t, err)
		mutateAndSave(t, img1)

		img2, err := local.NewImage(imageName2, dockerClient, local.FromBaseImage(runnableBaseImageName))
		h.AssertNil(t, err)
		mutateAndSave(t, img2)
		h.AssertNil(t, h.PushImage(dockerClient, imageName2, localTestRegistry.DockerRegistryAuth()))

		compare(t, imageName1, imageName2, localTestRegistry.GGCRKeychain())
	})
}

func randomLayer(t *testing.T, osType string) string {
	tr, err := h.CreateSingleFileTar(fmt.Sprintf("/new-layer-%s.txt", h.RandString(10)), "new-layer-"+h.RandString(10), osType)
	h.AssertNil(t, err)

	tarFile, err := ioutil.TempFile("", "add-layer-test")
	h.AssertNil(t, err)
	defer tarFile.Close()

	_, err = io.Copy(tarFile, tr)
	h.AssertNil(t, err)
	return tarFile.Name()
}

func compare(t *testing.T, img1, img2 string, keychain ggcrauthn.Keychain) {
	ref1, err := ggcrname.ParseReference(img1, ggcrname.WeakValidation)
	h.AssertNil(t, err)

	ref2, err := ggcrname.ParseReference(img2, ggcrname.WeakValidation)
	h.AssertNil(t, err)

	v1img1, err := ggcrremote.Image(ref1, ggcrremote.WithAuthFromKeychain(keychain))
	h.AssertNil(t, err)

	v1img2, err := ggcrremote.Image(ref2, ggcrremote.WithAuthFromKeychain(keychain))
	h.AssertNil(t, err)

	cfg1, err := v1img1.ConfigFile()
	h.AssertNil(t, err)

	cfg2, err := v1img2.ConfigFile()
	h.AssertNil(t, err)

	h.AssertEq(t, cfg1, cfg2)

	h.AssertEq(t, ref1.Identifier(), ref2.Identifier())
}
