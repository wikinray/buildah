package imagebuildah

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containers/buildah"
	"github.com/containers/buildah/pkg/parse"
	"github.com/containers/buildah/util"
	"github.com/containers/common/pkg/config"
	"github.com/containers/image/v5/docker/reference"
	is "github.com/containers/image/v5/storage"
	"github.com/containers/image/v5/transports"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	encconfig "github.com/containers/ocicrypt/config"
	"github.com/containers/storage"
	"github.com/containers/storage/pkg/archive"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/openshift/imagebuilder"
	"github.com/openshift/imagebuilder/dockerfile/parser"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/semaphore"
)

// builtinAllowedBuildArgs is list of built-in allowed build args.  Normally we
// complain if we're given values for arguments which have no corresponding ARG
// instruction in the Dockerfile, since that's usually an indication of a user
// error, but for these values we make exceptions and ignore them.
var builtinAllowedBuildArgs = map[string]bool{
	"HTTP_PROXY":  true,
	"http_proxy":  true,
	"HTTPS_PROXY": true,
	"https_proxy": true,
	"FTP_PROXY":   true,
	"ftp_proxy":   true,
	"NO_PROXY":    true,
	"no_proxy":    true,
}

// Executor is a buildah-based implementation of the imagebuilder.Executor
// interface.  It coordinates the entire build by using one or more
// StageExecutors to handle each stage of the build.
type Executor struct {
	stages                         map[string]*StageExecutor
	store                          storage.Store
	contextDir                     string
	pullPolicy                     buildah.PullPolicy
	registry                       string
	ignoreUnrecognizedInstructions bool
	quiet                          bool
	runtime                        string
	runtimeArgs                    []string
	transientMounts                []Mount
	compression                    archive.Compression
	output                         string
	outputFormat                   string
	additionalTags                 []string
	log                            func(format string, args ...interface{})
	in                             io.Reader
	out                            io.Writer
	err                            io.Writer
	signaturePolicyPath            string
	systemContext                  *types.SystemContext
	reportWriter                   io.Writer
	isolation                      buildah.Isolation
	namespaceOptions               []buildah.NamespaceOption
	configureNetwork               buildah.NetworkConfigurationPolicy
	cniPluginPath                  string
	cniConfigDir                   string
	idmappingOptions               *buildah.IDMappingOptions
	commonBuildOptions             *buildah.CommonBuildOptions
	defaultMountsFilePath          string
	iidfile                        string
	squash                         bool
	labels                         []string
	annotations                    []string
	layers                         bool
	useCache                       bool
	removeIntermediateCtrs         bool
	forceRmIntermediateCtrs        bool
	imageMap                       map[string]string           // Used to map images that we create to handle the AS construct.
	containerMap                   map[string]*buildah.Builder // Used to map from image names to only-created-for-the-rootfs containers.
	baseMap                        map[string]bool             // Holds the names of every base image, as given.
	rootfsMap                      map[string]bool             // Holds the names of every stage whose rootfs is referenced in a COPY or ADD instruction.
	blobDirectory                  string
	excludes                       []string
	unusedArgs                     map[string]struct{}
	capabilities                   []string
	devices                        []configs.Device
	signBy                         string
	architecture                   string
	os                             string
	maxPullPushRetries             int
	retryPullPushDelay             time.Duration
	ociDecryptConfig               *encconfig.DecryptConfig
	lastError                      error
	terminatedStage                map[string]struct{}
	stagesLock                     sync.Mutex
	stagesSemaphore                *semaphore.Weighted
	jobs                           int
}

// NewExecutor creates a new instance of the imagebuilder.Executor interface.
func NewExecutor(store storage.Store, options BuildOptions, mainNode *parser.Node) (*Executor, error) {
	defaultContainerConfig, err := config.Default()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get container config")
	}

	excludes, err := imagebuilder.ParseDockerignore(options.ContextDirectory)
	if err != nil {
		return nil, err
	}
	capabilities, err := defaultContainerConfig.Capabilities("", options.AddCapabilities, options.DropCapabilities)
	if err != nil {
		return nil, err
	}

	devices := []configs.Device{}
	for _, device := range append(defaultContainerConfig.Containers.Devices, options.Devices...) {
		dev, err := parse.DeviceFromPath(device)
		if err != nil {
			return nil, err
		}
		devices = append(dev, devices...)
	}

	transientMounts := []Mount{}
	for _, volume := range append(defaultContainerConfig.Containers.Volumes, options.TransientMounts...) {
		mount, err := parse.Volume(volume)
		if err != nil {
			return nil, err
		}

		transientMounts = append([]Mount{Mount(mount)}, transientMounts...)
	}

	exec := Executor{
		store:                          store,
		contextDir:                     options.ContextDirectory,
		excludes:                       excludes,
		pullPolicy:                     options.PullPolicy,
		registry:                       options.Registry,
		ignoreUnrecognizedInstructions: options.IgnoreUnrecognizedInstructions,
		quiet:                          options.Quiet,
		runtime:                        options.Runtime,
		runtimeArgs:                    options.RuntimeArgs,
		transientMounts:                transientMounts,
		compression:                    options.Compression,
		output:                         options.Output,
		outputFormat:                   options.OutputFormat,
		additionalTags:                 options.AdditionalTags,
		signaturePolicyPath:            options.SignaturePolicyPath,
		systemContext:                  options.SystemContext,
		log:                            options.Log,
		in:                             options.In,
		out:                            options.Out,
		err:                            options.Err,
		reportWriter:                   options.ReportWriter,
		isolation:                      options.Isolation,
		namespaceOptions:               options.NamespaceOptions,
		configureNetwork:               options.ConfigureNetwork,
		cniPluginPath:                  options.CNIPluginPath,
		cniConfigDir:                   options.CNIConfigDir,
		idmappingOptions:               options.IDMappingOptions,
		commonBuildOptions:             options.CommonBuildOpts,
		defaultMountsFilePath:          options.DefaultMountsFilePath,
		iidfile:                        options.IIDFile,
		squash:                         options.Squash,
		labels:                         append([]string{}, options.Labels...),
		annotations:                    append([]string{}, options.Annotations...),
		layers:                         options.Layers,
		useCache:                       !options.NoCache,
		removeIntermediateCtrs:         options.RemoveIntermediateCtrs,
		forceRmIntermediateCtrs:        options.ForceRmIntermediateCtrs,
		imageMap:                       make(map[string]string),
		containerMap:                   make(map[string]*buildah.Builder),
		baseMap:                        make(map[string]bool),
		rootfsMap:                      make(map[string]bool),
		blobDirectory:                  options.BlobDirectory,
		unusedArgs:                     make(map[string]struct{}),
		capabilities:                   capabilities,
		devices:                        devices,
		signBy:                         options.SignBy,
		architecture:                   options.Architecture,
		os:                             options.OS,
		maxPullPushRetries:             options.MaxPullPushRetries,
		retryPullPushDelay:             options.PullPushRetryDelay,
		ociDecryptConfig:               options.OciDecryptConfig,
		terminatedStage:                make(map[string]struct{}),
		jobs:                           options.Jobs,
	}
	if exec.err == nil {
		exec.err = os.Stderr
	}
	if exec.out == nil {
		exec.out = os.Stdout
	}
	if exec.log == nil {
		stepCounter := 0
		exec.log = func(format string, args ...interface{}) {
			stepCounter++
			prefix := fmt.Sprintf("STEP %d: ", stepCounter)
			suffix := "\n"
			fmt.Fprintf(exec.out, prefix+format+suffix, args...)
		}
	}
	for arg := range options.Args {
		if _, isBuiltIn := builtinAllowedBuildArgs[arg]; !isBuiltIn {
			exec.unusedArgs[arg] = struct{}{}
		}
	}
	for _, line := range mainNode.Children {
		node := line
		for node != nil { // tokens on this line, though we only care about the first
			switch strings.ToUpper(node.Value) { // first token - instruction
			case "ARG":
				arg := node.Next
				if arg != nil {
					// We have to be careful here - it's either an argument
					// and value, or just an argument, since they can be
					// separated by either "=" or whitespace.
					list := strings.SplitN(arg.Value, "=", 2)
					if _, stillUnused := exec.unusedArgs[list[0]]; stillUnused {
						delete(exec.unusedArgs, list[0])
					}
				}
			}
			break
		}
	}
	return &exec, nil
}

// startStage creates a new stage executor that will be referenced whenever a
// COPY or ADD statement uses a --from=NAME flag.
func (b *Executor) startStage(stage *imagebuilder.Stage, stages int, output string) *StageExecutor {
	if b.stages == nil {
		b.stages = make(map[string]*StageExecutor)
	}
	stageExec := &StageExecutor{
		executor:        b,
		index:           stage.Position,
		stages:          stages,
		name:            stage.Name,
		volumeCache:     make(map[string]string),
		volumeCacheInfo: make(map[string]os.FileInfo),
		output:          output,
		stage:           stage,
	}
	b.stages[stage.Name] = stageExec
	if idx := strconv.Itoa(stage.Position); idx != stage.Name {
		b.stages[idx] = stageExec
	}
	return stageExec
}

// resolveNameToImageRef creates a types.ImageReference for the output name in local storage
func (b *Executor) resolveNameToImageRef(output string) (types.ImageReference, error) {
	imageRef, err := alltransports.ParseImageName(output)
	if err != nil {
		candidates, _, _, err := util.ResolveName(output, "", b.systemContext, b.store)
		if err != nil {
			return nil, errors.Wrapf(err, "error parsing target image name %q", output)
		}
		if len(candidates) == 0 {
			return nil, errors.Errorf("error parsing target image name %q", output)
		}
		imageRef2, err2 := is.Transport.ParseStoreReference(b.store, candidates[0])
		if err2 != nil {
			return nil, errors.Wrapf(err, "error parsing target image name %q", output)
		}
		return imageRef2, nil
	}
	return imageRef, nil
}

func (b *Executor) waitForStage(ctx context.Context, name string) error {
	stage := b.stages[name]
	if stage == nil {
		return errors.Errorf("unknown stage %q", name)
	}
	for {
		if b.lastError != nil {
			return b.lastError
		}
		if stage.stage == nil {
			return nil
		}

		b.stagesLock.Lock()
		_, terminated := b.terminatedStage[name]
		b.stagesLock.Unlock()

		if terminated {
			return nil
		}

		b.stagesSemaphore.Release(1)
		time.Sleep(time.Millisecond * 10)
		if err := b.stagesSemaphore.Acquire(ctx, 1); err != nil {
			return err
		}
	}
}

// getImageHistory returns the history of imageID.
func (b *Executor) getImageHistory(ctx context.Context, imageID string) ([]v1.History, error) {
	imageRef, err := is.Transport.ParseStoreReference(b.store, "@"+imageID)
	if err != nil {
		return nil, errors.Wrapf(err, "error getting image reference %q", imageID)
	}
	ref, err := imageRef.NewImage(ctx, nil)
	if err != nil {
		return nil, errors.Wrapf(err, "error creating new image from reference to image %q", imageID)
	}
	defer ref.Close()
	oci, err := ref.OCIConfig(ctx)
	if err != nil {
		return nil, errors.Wrapf(err, "error getting possibly-converted OCI config of image %q", imageID)
	}
	return oci.History, nil
}

func (b *Executor) buildStage(ctx context.Context, cleanupStages map[int]*StageExecutor, stages imagebuilder.Stages, stageIndex int) (imageID string, ref reference.Canonical, err error) {
	stage := stages[stageIndex]
	ib := stage.Builder
	node := stage.Node
	base, err := ib.From(node)

	// If this is the last stage, then the image that we produce at
	// its end should be given the desired output name.
	output := ""
	if stageIndex == len(stages)-1 {
		output = b.output
	}

	if err != nil {
		logrus.Debugf("Build(node.Children=%#v)", node.Children)
		return "", nil, err
	}

	stageExecutor := b.startStage(&stage, len(stages), output)

	// If this a single-layer build, or if it's a multi-layered
	// build and b.forceRmIntermediateCtrs is set, make sure we
	// remove the intermediate/build containers, regardless of
	// whether or not the stage's build fails.
	if b.forceRmIntermediateCtrs || !b.layers {
		b.stagesLock.Lock()
		cleanupStages[stage.Position] = stageExecutor
		b.stagesLock.Unlock()
	}

	// Build this stage.
	if imageID, ref, err = stageExecutor.Execute(ctx, base); err != nil {
		return "", nil, err
	}

	// The stage succeeded, so remove its build container if we're
	// told to delete successful intermediate/build containers for
	// multi-layered builds.
	if b.removeIntermediateCtrs {
		b.stagesLock.Lock()
		cleanupStages[stage.Position] = stageExecutor
		b.stagesLock.Unlock()
	}

	return imageID, ref, nil
}

// Build takes care of the details of running Prepare/Execute/Commit/Delete
// over each of the one or more parsed Dockerfiles and stages.
func (b *Executor) Build(ctx context.Context, stages imagebuilder.Stages) (imageID string, ref reference.Canonical, err error) {
	if len(stages) == 0 {
		return "", nil, errors.New("error building: no stages to build")
	}
	var cleanupImages []string
	cleanupStages := make(map[int]*StageExecutor)

	stdout := b.out
	if b.quiet {
		b.out = ioutil.Discard
	}

	cleanup := func() error {
		var lastErr error
		// Clean up any containers associated with the final container
		// built by a stage, for stages that succeeded, since we no
		// longer need their filesystem contents.

		b.stagesLock.Lock()
		for _, stage := range cleanupStages {
			if err := stage.Delete(); err != nil {
				logrus.Debugf("Failed to cleanup stage containers: %v", err)
				lastErr = err
			}
		}
		b.stagesLock.Unlock()

		cleanupStages = nil
		// Clean up any builders that we used to get data from images.
		for _, builder := range b.containerMap {
			if err := builder.Delete(); err != nil {
				logrus.Debugf("Failed to cleanup image containers: %v", err)
				lastErr = err
			}
		}
		b.containerMap = nil
		// Clean up any intermediate containers associated with stages,
		// since we're not keeping them for debugging.
		if b.removeIntermediateCtrs {
			if err := b.deleteSuccessfulIntermediateCtrs(); err != nil {
				logrus.Debugf("Failed to cleanup intermediate containers: %v", err)
				lastErr = err
			}
		}
		// Remove images from stages except the last one, since we're
		// not going to use them as a starting point for any new
		// stages.
		for i := range cleanupImages {
			removeID := cleanupImages[len(cleanupImages)-i-1]
			if removeID == imageID {
				continue
			}
			if _, err := b.store.DeleteImage(removeID, true); err != nil {
				logrus.Debugf("failed to remove intermediate image %q: %v", removeID, err)
				if b.forceRmIntermediateCtrs || errors.Cause(err) != storage.ErrImageUsedByContainer {
					lastErr = err
				}
			}
		}
		cleanupImages = nil
		return lastErr
	}

	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			if err == nil {
				err = cleanupErr
			} else {
				err = errors.Wrap(err, cleanupErr.Error())
			}
		}
	}()

	// Build maps of every named base image and every referenced stage root
	// filesystem.  Individual stages can use them to determine whether or
	// not they can skip certain steps near the end of their stages.
	for _, stage := range stages {
		node := stage.Node // first line
		for node != nil {  // each line
			for _, child := range node.Children { // tokens on this line, though we only care about the first
				switch strings.ToUpper(child.Value) { // first token - instruction
				case "FROM":
					if child.Next != nil { // second token on this line
						base := child.Next.Value
						if base != "scratch" {
							// TODO: this didn't undergo variable and arg
							// expansion, so if the AS clause in another
							// FROM instruction uses argument values,
							// we might not record the right value here.
							b.baseMap[base] = true
							logrus.Debugf("base: %q", base)
						}
					}
				case "ADD", "COPY":
					for _, flag := range child.Flags { // flags for this instruction
						if strings.HasPrefix(flag, "--from=") {
							// TODO: this didn't undergo variable and
							// arg expansion, so if the previous stage
							// was named using argument values, we might
							// not record the right value here.
							rootfs := strings.TrimPrefix(flag, "--from=")
							b.rootfsMap[rootfs] = true
							logrus.Debugf("rootfs: %q", rootfs)
						}
					}
				}
			}
			node = node.Next // next line
		}
	}

	type Result struct {
		Index   int
		ImageID string
		Ref     reference.Canonical
		Error   error
	}

	ch := make(chan Result)

	jobs := int64(b.jobs)
	if jobs < 0 {
		return "", nil, errors.New("error building: invalid value for jobs.  It must be a positive integer")
	} else if jobs == 0 {
		jobs = int64(len(stages))
	}

	b.stagesSemaphore = semaphore.NewWeighted(jobs)

	var wg sync.WaitGroup
	wg.Add(len(stages))

	go func() {
		for stageIndex := range stages {
			index := stageIndex
			// Acquire the sempaphore before creating the goroutine so we are sure they
			// run in the specified order.
			if err := b.stagesSemaphore.Acquire(ctx, 1); err != nil {
				b.lastError = err
				return
			}
			go func() {
				defer b.stagesSemaphore.Release(1)
				defer wg.Done()
				imageID, ref, err = b.buildStage(ctx, cleanupStages, stages, index)
				if err != nil {
					ch <- Result{
						Index: index,
						Error: err,
					}
					return
				}

				ch <- Result{
					Index:   index,
					ImageID: imageID,
					Ref:     ref,
					Error:   nil,
				}
			}()
		}
	}()
	go func() {
		wg.Wait()
		close(ch)
	}()

	for r := range ch {
		stage := stages[r.Index]

		b.stagesLock.Lock()
		b.terminatedStage[stage.Name] = struct{}{}
		b.stagesLock.Unlock()

		if r.Error != nil {
			b.lastError = r.Error
			return "", nil, r.Error
		}

		// If this is an intermediate stage, make a note of the ID, so
		// that we can look it up later.
		if r.Index < len(stages)-1 && r.ImageID != "" {
			b.imageMap[stage.Name] = r.ImageID
			// We're not populating the cache with intermediate
			// images, so add this one to the list of images that
			// we'll remove later.
			if !b.layers {
				cleanupImages = append(cleanupImages, r.ImageID)
			}
		}
		if r.Index == len(stages)-1 {
			imageID = r.ImageID
		}
	}

	if len(b.unusedArgs) > 0 {
		unusedList := make([]string, 0, len(b.unusedArgs))
		for k := range b.unusedArgs {
			unusedList = append(unusedList, k)
		}
		sort.Strings(unusedList)
		fmt.Fprintf(b.out, "[Warning] one or more build args were not consumed: %v\n", unusedList)
	}

	if len(b.additionalTags) > 0 {
		if dest, err := b.resolveNameToImageRef(b.output); err == nil {
			switch dest.Transport().Name() {
			case is.Transport.Name():
				img, err := is.Transport.GetStoreImage(b.store, dest)
				if err != nil {
					return imageID, ref, errors.Wrapf(err, "error locating just-written image %q", transports.ImageName(dest))
				}
				if err = util.AddImageNames(b.store, "", b.systemContext, img, b.additionalTags); err != nil {
					return imageID, ref, errors.Wrapf(err, "error setting image names to %v", append(img.Names, b.additionalTags...))
				}
				logrus.Debugf("assigned names %v to image %q", img.Names, img.ID)
			default:
				logrus.Warnf("don't know how to add tags to images stored in %q transport", dest.Transport().Name())
			}
		}
	}

	if err := cleanup(); err != nil {
		return "", nil, err
	}
	logrus.Debugf("printing final image id %q", imageID)
	if b.iidfile != "" {
		if err = ioutil.WriteFile(b.iidfile, []byte(imageID), 0644); err != nil {
			return imageID, ref, errors.Wrapf(err, "failed to write image ID to file %q", b.iidfile)
		}
	} else {
		if _, err := stdout.Write([]byte(imageID + "\n")); err != nil {
			return imageID, ref, errors.Wrapf(err, "failed to write image ID to stdout")
		}
	}
	return imageID, ref, nil
}

// deleteSuccessfulIntermediateCtrs goes through the container IDs in each
// stage's containerIDs list and deletes the containers associated with those
// IDs.
func (b *Executor) deleteSuccessfulIntermediateCtrs() error {
	var lastErr error
	for _, s := range b.stages {
		for _, ctr := range s.containerIDs {
			if err := b.store.DeleteContainer(ctr); err != nil {
				logrus.Errorf("error deleting build container %q: %v\n", ctr, err)
				lastErr = err
			}
		}
		// The stages map includes some stages under multiple keys, so
		// clearing their lists after we process a given stage is
		// necessary to avoid triggering errors that would occur if we
		// tried to delete a given stage's containers multiple times.
		s.containerIDs = nil
	}
	return lastErr
}
