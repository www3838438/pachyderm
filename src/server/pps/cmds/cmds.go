package cmds

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/fsouza/go-dockerclient"
	"github.com/gogo/protobuf/jsonpb"
	pach "github.com/pachyderm/pachyderm/src/client"
	pfsclient "github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/client/pkg/uuid"
	ppsclient "github.com/pachyderm/pachyderm/src/client/pps"
	"github.com/pachyderm/pachyderm/src/server/pkg/cmdutil"
	"github.com/pachyderm/pachyderm/src/server/pps/example"
	"github.com/pachyderm/pachyderm/src/server/pps/pretty"
	"github.com/spf13/cobra"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

const (
	codestart = "```sh"
	codeend   = "```"
)

// Cmds returns a slice containing pps commands.
func Cmds(address string, noMetrics *bool) ([]*cobra.Command, error) {
	metrics := !*noMetrics
	raw := false
	rawFlag := func(cmd *cobra.Command) {
		cmd.Flags().BoolVar(&raw, "raw", false, "disable pretty printing, print raw json")
	}
	marshaller := &jsonpb.Marshaler{Indent: "  "}

	job := &cobra.Command{
		Use:   "job",
		Short: "Docs for jobs.",
		Long: `Jobs are the basic unit of computation in Pachyderm.

Jobs run a containerized workload over a set of finished input commits.
Creating a job will also create a new repo and a commit in that repo which
contains the output of the job. Unless the job is created with another job as a
parent. If the job is created with a parent it will use the same repo as its
parent job and the commit it creates will use the parent job's commit as a
parent.
If the job fails the commit it creates will not be finished.
The increase the throughput of a job increase the Shard paremeter.
`,
		Run: cmdutil.RunFixedArgs(0, func(args []string) error {
			return nil
		}),
	}

	exampleCreateJobRequest, err := marshaller.MarshalToString(example.CreateJobRequest)
	if err != nil {
		return nil, err
	}

	exampleRunPipelineSpec, err := marshaller.MarshalToString(example.RunPipelineSpec)
	if err != nil {
		return nil, err
	}

	pipelineSpec := "[Pipeline Specification](../reference/pipeline_spec.html)"

	var jobPath string
	var pushImages bool
	var registry string
	var username string
	var password string
	createJob := &cobra.Command{
		Use:   "create-job -f job.json",
		Short: "Create a new job. Returns the id of the created job.",
		Long:  fmt.Sprintf("Create a new job from a spec, the spec looks like this\n%s", exampleCreateJobRequest),
		Run: cmdutil.RunFixedArgs(0, func(args []string) (retErr error) {
			client, err := pach.NewMetricsClientFromAddress(address, metrics, "user")
			if err != nil {
				return err
			}
			var buf bytes.Buffer
			var jobReader io.Reader
			if jobPath == "-" {
				jobReader = io.TeeReader(os.Stdin, &buf)
				fmt.Print("Reading from stdin.\n")
			} else if url, err := url.Parse(jobPath); err == nil && url.Scheme != "" {
				resp, err := http.Get(url.String())
				if err != nil {
					return sanitizeErr(err)
				}
				defer func() {
					if err := resp.Body.Close(); err != nil && retErr == nil {
						retErr = sanitizeErr(err)
					}
				}()
				jobReader = resp.Body
			} else {
				jobFile, err := os.Open(jobPath)
				if err != nil {
					return sanitizeErr(err)
				}
				defer func() {
					if err := jobFile.Close(); err != nil && retErr == nil {
						retErr = sanitizeErr(err)
					}
				}()
				jobReader = io.TeeReader(jobFile, &buf)
			}
			var request ppsclient.CreateJobRequest
			decoder := json.NewDecoder(jobReader)
			if err := jsonpb.UnmarshalNext(decoder, &request); err != nil {
				return sanitizeErr(err)
			}
			if len(request.Inputs) != 0 {
				fmt.Printf("WARNING: field `inputs` is deprecated, use `input` instead.\n")
			}
			if pushImages {
				pushedImage, err := pushImage(registry, username, password, request.Transform.Image)
				if err != nil {
					return err
				}
				request.Transform.Image = pushedImage
			}
			job, err := client.PpsAPIClient.CreateJob(
				context.Background(),
				&request,
			)
			if err != nil {
				return sanitizeErr(err)
			}
			fmt.Println(job.ID)
			return nil
		}),
	}
	createJob.Flags().StringVarP(&jobPath, "file", "f", "-", "The file containing the job, it can be a url or local file. - reads from stdin.")
	createJob.Flags().BoolVarP(&pushImages, "push-images", "p", false, "If true, push local docker images into the cluster registry.")
	createJob.Flags().StringVarP(&registry, "registry", "r", "docker.io", "The registry to push images to.")
	createJob.Flags().StringVarP(&username, "username", "u", "", "The username to push images as, defaults to your OS username.")
	createJob.Flags().StringVarP(&password, "password", "", "", "Your password for the registry being pushed to.")

	var block bool
	inspectJob := &cobra.Command{
		Use:   "inspect-job job-id",
		Short: "Return info about a job.",
		Long:  "Return info about a job.",
		Run: cmdutil.RunFixedArgs(1, func(args []string) error {
			client, err := pach.NewMetricsClientFromAddress(address, metrics, "user")
			if err != nil {
				return err
			}
			jobInfo, err := client.InspectJob(args[0], block)
			if err != nil {
				cmdutil.ErrorAndExit("error from InspectJob: %s", err.Error())
			}
			if jobInfo == nil {
				cmdutil.ErrorAndExit("job %s not found.", args[0])
			}
			if raw {
				return marshaller.Marshal(os.Stdout, jobInfo)
			}
			return pretty.PrintDetailedJobInfo(jobInfo)
		}),
	}
	inspectJob.Flags().BoolVarP(&block, "block", "b", false, "block until the job has either succeeded or failed")
	rawFlag(inspectJob)

	var pipelineName string
	listJob := &cobra.Command{
		Use:   "list-job [-p pipeline-name] [commits]",
		Short: "Return info about jobs.",
		Long: `Return info about jobs.

Examples:

	` + codestart + `# return all jobs
	$ pachctl list-job

	# return all jobs in pipeline foo
	$ pachctl list-job -p foo

	# return all jobs whose input commits include foo/XXX and bar/YYY
	$ pachctl list-job foo/XXX bar/YYY

	# return all jobs in pipeline foo and whose input commits include bar/YYY
	$ pachctl list-job -p foo bar/YYY
` + codeend,
		Run: cmdutil.RunFixedArgs(0, func(args []string) error {
			client, err := pach.NewMetricsClientFromAddress(address, metrics, "user")
			if err != nil {
				return err
			}

			commits, err := cmdutil.ParseCommits(args)
			if err != nil {
				return err
			}

			jobInfos, err := client.ListJob(pipelineName, commits)
			if err != nil {
				return sanitizeErr(err)
			}

			// Display newest jobs first
			sort.Sort(sort.Reverse(ByCreationTime(jobInfos)))

			if raw {
				for _, jobInfo := range jobInfos {
					if err := marshaller.Marshal(os.Stdout, jobInfo); err != nil {
						return err
					}
				}
				return nil
			}
			writer := tabwriter.NewWriter(os.Stdout, 0, 1, 1, ' ', 0)
			pretty.PrintJobHeader(writer)
			for _, jobInfo := range jobInfos {
				pretty.PrintJobInfo(writer, jobInfo)
			}

			return writer.Flush()
		}),
	}
	listJob.Flags().StringVarP(&pipelineName, "pipeline", "p", "", "Limit to jobs made by pipeline.")
	rawFlag(listJob)

	deleteJob := &cobra.Command{
		Use:   "delete-job job-id",
		Short: "Delete a job.",
		Long:  "Delete a job.",
		Run: cmdutil.RunFixedArgs(1, func(args []string) error {
			client, err := pach.NewMetricsClientFromAddress(address, metrics, "user")
			if err != nil {
				return err
			}
			if err := client.DeleteJob(args[0]); err != nil {
				cmdutil.ErrorAndExit("error from DeleteJob: %s", err.Error())
			}
			return nil
		}),
	}

	stopJob := &cobra.Command{
		Use:   "stop-job job-id",
		Short: "Stop a job.",
		Long:  "Stop a job.  The job will be stopped immediately.",
		Run: cmdutil.RunFixedArgs(1, func(args []string) error {
			client, err := pach.NewMetricsClientFromAddress(address, metrics, "user")
			if err != nil {
				return err
			}
			if err := client.StopJob(args[0]); err != nil {
				cmdutil.ErrorAndExit("error from StopJob: %s", err.Error())
			}
			return nil
		}),
	}

	restartDatum := &cobra.Command{
		Use:   "restart-datum job-id datum-path1,datum-path2",
		Short: "Restart a datum.",
		Long:  "Restart a datum.",
		Run: cmdutil.RunFixedArgs(2, func(args []string) error {
			client, err := pach.NewMetricsClientFromAddress(address, metrics, "user")
			if err != nil {
				return fmt.Errorf("error from GetLogs: %v", sanitizeErr(err))
			}
			datumFilter := strings.Split(args[1], ",")
			for i := 0; i < len(datumFilter); {
				if len(datumFilter[i]) == 0 {
					if i+1 < len(datumFilter) {
						copy(datumFilter[i:], datumFilter[i+1:])
					}
					datumFilter = datumFilter[:len(datumFilter)-1]
				} else {
					i++
				}
			}
			if err := client.RestartDatum(args[0], datumFilter); err != nil {
				return sanitizeErr(err)
			}
			return nil
		}),
	}

	var (
		jobID       string
		commaInputs string // comma-separated list of input files of interest
	)
	getLogs := &cobra.Command{
		Use:   "get-logs [--pipeline=<pipeline>|--job=<job id>]",
		Short: "Return logs from a job.",
		Long: `Return logs from a job.

Examples:

	` + codestart + `# return logs emitted by recent jobs in the "filter" pipeline
	$ pachctl get-logs --pipeline=filter

	# return logs emitted by the job aedfa12aedf
	$ pachctl get-logs --job=aedfa12aedf

	# return logs emitted by the pipeline \"filter\" while processing /apple.txt and a file with the hash 123aef
	$ pachctl get-logs --pipeline=filter --inputs=/apple.txt,123aef
` + codeend,
		Run: cmdutil.RunFixedArgs(0, func(args []string) error {
			client, err := pach.NewMetricsClientFromAddress(address, metrics, "user")
			if err != nil {
				return fmt.Errorf("error from GetLogs: %v", sanitizeErr(err))
			}
			// Validate flags
			if len(jobID) == 0 && len(pipelineName) == 0 {
				return fmt.Errorf("must set either --pipeline or --job (or both)")
			}

			// Break up comma-separated input paths, and filter out empty entries
			data := strings.Split(commaInputs, ",")
			for i := 0; i < len(data); {
				if len(data[i]) == 0 {
					if i+1 < len(data) {
						copy(data[i:], data[i+1:])
					}
					data = data[:len(data)-1]
				} else {
					i++
				}
			}

			// Issue RPC
			marshaler := &jsonpb.Marshaler{}
			iter := client.GetLogs(pipelineName, jobID, data)
			for iter.Next() {
				var messageStr string
				if raw {
					var err error
					messageStr, err = marshaler.MarshalToString(iter.Message())
					if err != nil {
						fmt.Fprintf(os.Stderr, "error marshalling \"%v\": %s\n", iter.Message(), err)
					}
				} else {
					if iter.Message().User {
						messageStr = iter.Message().Message
					}
				}
				if messageStr != "" {
					fmt.Println(messageStr)
				}
			}
			return iter.Err()
		}),
	}
	getLogs.Flags().StringVar(&pipelineName, "pipeline", "", "Filter the log "+
		"for lines from this pipeline (accepts pipeline name)")
	getLogs.Flags().StringVar(&jobID, "job", "", "Filter for log lines from "+
		"this job (accepts job ID)")
	getLogs.Flags().StringVar(&commaInputs, "inputs", "", "Filter for log lines "+
		"generated while processing these files (accepts PFS paths or file hashes)")
	getLogs.Flags().BoolVar(&raw, "raw", false, "Return log messages verbatim from server.")

	pipeline := &cobra.Command{
		Use:   "pipeline",
		Short: "Docs for pipelines.",
		Long: `Pipelines are a powerful abstraction for automating jobs.

Pipelines take a set of repos as inputs, rather than the set of commits that
jobs take. Pipelines then subscribe to commits on those repos and launches a job
to process each incoming commit.
Creating a pipeline will also create a repo of the same name.
All jobs created by a pipeline will create commits in the pipeline's repo.
`,
		Run: cmdutil.RunFixedArgs(0, func(args []string) error {
			return nil
		}),
	}

	var pipelinePath string
	var description string
	createPipeline := &cobra.Command{
		Use:   "create-pipeline -f pipeline.json",
		Short: "Create a new pipeline.",
		Long:  fmt.Sprintf("Create a new pipeline from a %s", pipelineSpec),
		Run: cmdutil.RunFixedArgs(0, func(args []string) (retErr error) {
			cfgReader, err := newPipelineManifestReader(pipelinePath)
			if err != nil {
				return err
			}
			client, err := pach.NewMetricsClientFromAddress(address, metrics, "user")
			if err != nil {
				return sanitizeErr(err)
			}
			for {
				request, err := cfgReader.nextCreatePipelineRequest()
				if err == io.EOF {
					break
				} else if err != nil {
					return err
				}
				if len(request.Inputs) != 0 {
					fmt.Printf("WARNING: field `inputs` is deprecated, use `input` instead.\n")
				}
				if pushImages {
					pushedImage, err := pushImage(registry, username, password, request.Transform.Image)
					if err != nil {
						return err
					}
					request.Transform.Image = pushedImage
				}
				if _, err := client.PpsAPIClient.CreatePipeline(
					context.Background(),
					request,
				); err != nil {
					return sanitizeErr(err)
				}
			}
			return nil
		}),
	}
	createPipeline.Flags().StringVarP(&pipelinePath, "file", "f", "-", "The file containing the pipeline, it can be a url or local file. - reads from stdin.")
	createPipeline.Flags().BoolVarP(&pushImages, "push-images", "p", false, "If true, push local docker images into the cluster registry.")
	createPipeline.Flags().StringVarP(&registry, "registry", "r", "docker.io", "The registry to push images to.")
	createPipeline.Flags().StringVarP(&username, "username", "u", "", "The username to push images as, defaults to your OS username.")
	createPipeline.Flags().StringVarP(&password, "password", "", "", "Your password for the registry being pushed to.")
	createPipeline.Flags().StringVarP(&description, "description", "d", "", "A description of the repo.")

	updatePipeline := &cobra.Command{
		Use:   "update-pipeline -f pipeline.json",
		Short: "Update an existing Pachyderm pipeline.",
		Long:  fmt.Sprintf("Update a Pachyderm pipeline with a new %s", pipelineSpec),
		Run: cmdutil.RunFixedArgs(0, func(args []string) (retErr error) {
			cfgReader, err := newPipelineManifestReader(pipelinePath)
			if err != nil {
				return err
			}
			client, err := pach.NewMetricsClientFromAddress(address, metrics, "user")
			if err != nil {
				return sanitizeErr(err)
			}
			for {
				request, err := cfgReader.nextCreatePipelineRequest()
				if err == io.EOF {
					break
				} else if err != nil {
					return err
				}
				request.Update = true
				if pushImages {
					pushedImage, err := pushImage(registry, username, password, request.Transform.Image)
					if err != nil {
						return err
					}
					request.Transform.Image = pushedImage
				}
				if _, err := client.PpsAPIClient.CreatePipeline(
					context.Background(),
					request,
				); err != nil {
					return sanitizeErr(err)
				}
			}
			return nil
		}),
	}
	updatePipeline.Flags().StringVarP(&pipelinePath, "file", "f", "-", "The file containing the pipeline, it can be a url or local file. - reads from stdin.")
	updatePipeline.Flags().BoolVarP(&pushImages, "push-images", "p", false, "If true, push local docker images into the cluster registry.")
	updatePipeline.Flags().StringVarP(&registry, "registry", "r", "docker.io", "The registry to push images to.")
	updatePipeline.Flags().StringVarP(&username, "username", "u", "", "The username to push images as, defaults to your OS username.")
	updatePipeline.Flags().StringVarP(&password, "password", "", "", "Your password for the registry being pushed to.")

	inspectPipeline := &cobra.Command{
		Use:   "inspect-pipeline pipeline-name",
		Short: "Return info about a pipeline.",
		Long:  "Return info about a pipeline.",
		Run: cmdutil.RunFixedArgs(1, func(args []string) error {
			client, err := pach.NewMetricsClientFromAddress(address, metrics, "user")
			if err != nil {
				return err
			}
			pipelineInfo, err := client.InspectPipeline(args[0])
			if err != nil {
				return sanitizeErr(err)
			}
			if pipelineInfo == nil {
				return fmt.Errorf("pipeline %s not found", args[0])
			}
			if raw {
				return marshaller.Marshal(os.Stdout, pipelineInfo)
			}
			return pretty.PrintDetailedPipelineInfo(pipelineInfo)
		}),
	}
	rawFlag(inspectPipeline)

	listPipeline := &cobra.Command{
		Use:   "list-pipeline",
		Short: "Return info about all pipelines.",
		Long:  "Return info about all pipelines.",
		Run: cmdutil.RunFixedArgs(0, func(args []string) error {
			client, err := pach.NewMetricsClientFromAddress(address, metrics, "user")
			if err != nil {
				return err
			}
			pipelineInfos, err := client.ListPipeline()
			if err != nil {
				return sanitizeErr(err)
			}
			if raw {
				for _, pipelineInfo := range pipelineInfos {
					if err := marshaller.Marshal(os.Stdout, pipelineInfo); err != nil {
						return err
					}
				}
				return nil
			}
			writer := tabwriter.NewWriter(os.Stdout, 20, 1, 3, ' ', 0)
			pretty.PrintPipelineHeader(writer)
			for _, pipelineInfo := range pipelineInfos {
				pretty.PrintPipelineInfo(writer, pipelineInfo)
			}
			return writer.Flush()
		}),
	}
	rawFlag(listPipeline)

	var deleteJobs bool
	deletePipeline := &cobra.Command{
		Use:   "delete-pipeline pipeline-name",
		Short: "Delete a pipeline.",
		Long:  "Delete a pipeline.",
		Run: cmdutil.RunFixedArgs(1, func(args []string) error {
			client, err := pach.NewMetricsClientFromAddress(address, metrics, "user")
			if err != nil {
				return err
			}
			if err := client.DeletePipeline(args[0], deleteJobs); err != nil {
				cmdutil.ErrorAndExit("error from DeletePipeline: %s", err.Error())
			}
			return nil
		}),
	}
	deletePipeline.Flags().BoolVar(&deleteJobs, "delete-jobs", false, "delete the jobs in this pipeline as well")

	startPipeline := &cobra.Command{
		Use:   "start-pipeline pipeline-name",
		Short: "Restart a stopped pipeline.",
		Long:  "Restart a stopped pipeline.",
		Run: cmdutil.RunFixedArgs(1, func(args []string) error {
			client, err := pach.NewMetricsClientFromAddress(address, metrics, "user")
			if err != nil {
				return err
			}
			if err := client.StartPipeline(args[0]); err != nil {
				cmdutil.ErrorAndExit("error from StartPipeline: %s", err.Error())
			}
			return nil
		}),
	}

	stopPipeline := &cobra.Command{
		Use:   "stop-pipeline pipeline-name",
		Short: "Stop a running pipeline.",
		Long:  "Stop a running pipeline.",
		Run: cmdutil.RunFixedArgs(1, func(args []string) error {
			client, err := pach.NewMetricsClientFromAddress(address, metrics, "user")
			if err != nil {
				return err
			}
			if err := client.StopPipeline(args[0]); err != nil {
				cmdutil.ErrorAndExit("error from StopPipeline: %s", err.Error())
			}
			return nil
		}),
	}

	var specPath string
	runPipeline := &cobra.Command{
		Use:   "run-pipeline pipeline-name [-f job.json]",
		Short: "Run a pipeline once.",
		Long:  fmt.Sprintf("Run a pipeline once, optionally overriding some pipeline options by providing a spec.  The spec looks like this:\n%s", exampleRunPipelineSpec),
		Run: cmdutil.RunFixedArgs(1, func(args []string) (retErr error) {
			client, err := pach.NewMetricsClientFromAddress(address, metrics, "user")
			if err != nil {
				return err
			}

			request := &ppsclient.CreateJobRequest{
				Pipeline: &ppsclient.Pipeline{
					Name: args[0],
				},
			}

			var buf bytes.Buffer
			var specReader io.Reader
			if specPath == "-" {
				specReader = io.TeeReader(os.Stdin, &buf)
				fmt.Print("Reading from stdin.\n")
			} else if specPath != "" {
				specFile, err := os.Open(specPath)
				if err != nil {
					return err
				}

				defer func() {
					if err := specFile.Close(); err != nil && retErr == nil {
						retErr = err
					}
				}()

				specReader = io.TeeReader(specFile, &buf)
				decoder := json.NewDecoder(specReader)
				if err := jsonpb.UnmarshalNext(decoder, request); err != nil {
					return err
				}
			}

			job, err := client.PpsAPIClient.CreateJob(
				context.Background(),
				request,
			)
			if err != nil {
				return sanitizeErr(err)
			}
			fmt.Println(job.ID)
			return nil
		}),
	}
	runPipeline.Flags().StringVarP(&specPath, "file", "f", "", "The file containing the run-pipeline spec, - reads from stdin.")

	var repos cmdutil.RepeatedStringArg
	flushCommit := &cobra.Command{
		Use:   "flush-commit commit [commit ...]",
		Short: "Wait for all commits caused by the specified commits to finish and return them.",
		Long: `Wait for all commits caused by the specified commits to finish and return them.

Examples:

` + codestart + `# return commits caused by foo/XXX and bar/YYY
$ pachctl flush-commit foo/XXX bar/YYY

# return commits caused by foo/XXX leading to repos bar and baz
$ pachctl flush-commit foo/XXX -r bar -r baz
` + codeend,
		Run: cmdutil.Run(func(args []string) error {
			commits, err := cmdutil.ParseCommits(args)
			if err != nil {
				return err
			}

			c, err := pach.NewMetricsClientFromAddress(address, metrics, "user")
			if err != nil {
				return err
			}

			var toRepos []*pfsclient.Repo
			for _, repoName := range repos {
				toRepos = append(toRepos, pach.NewRepo(repoName))
			}

			writer := tabwriter.NewWriter(os.Stdout, 20, 1, 3, ' ', 0)
			if !raw {
				pretty.PrintJobHeader(writer)
			}
			if err := c.FlushCommit(commits, toRepos, func(jobInfo *ppsclient.JobInfo) error {
				if raw {
					return marshaller.Marshal(os.Stdout, jobInfo)
				}
				pretty.PrintJobInfo(writer, jobInfo)
				return nil
			}); err != nil {
				return err
			}

			return writer.Flush()
		}),
	}
	flushCommit.Flags().VarP(&repos, "repos", "r", "Wait only for commits leading to a specific set of repos")
	rawFlag(flushCommit)

	garbageCollect := &cobra.Command{
		Use:   "garbage-collect",
		Short: "Garbage collect unused data.",
		Long: `Garbage collect unused data.

When a file/commit/repo is deleted, the data is not immediately removed from the underlying storage system (e.g. S3) for performance and architectural reasons.  This is similar to how when you delete a file on your computer, the file is not necessarily wiped from disk immediately.

To actually remove the data, you will need to manually invoke garbage collection.  The easiest way to do it is through "pachctl garbage-collecth".

Currently "pachctl garbage-collect" can only be started when there are no active jobs running.  You also need to ensure that there's no ongoing "put-file".  Garbage collection puts the cluster into a readonly mode where no new jobs can be created and no data can be added.
`,
		Run: cmdutil.RunFixedArgs(0, func(args []string) (retErr error) {
			client, err := pach.NewMetricsClientFromAddress(address, metrics, "user")
			if err != nil {
				return err
			}

			return client.GarbageCollect()
		}),
	}

	var result []*cobra.Command
	result = append(result, job)
	result = append(result, createJob)
	result = append(result, inspectJob)
	result = append(result, listJob)
	result = append(result, deleteJob)
	result = append(result, stopJob)
	result = append(result, restartDatum)
	result = append(result, getLogs)
	result = append(result, pipeline)
	result = append(result, createPipeline)
	result = append(result, updatePipeline)
	result = append(result, inspectPipeline)
	result = append(result, listPipeline)
	result = append(result, deletePipeline)
	result = append(result, startPipeline)
	result = append(result, stopPipeline)
	result = append(result, runPipeline)
	result = append(result, flushCommit)
	result = append(result, garbageCollect)
	return result, nil
}

// ByCreationTime is an implementation of sort.Interface which
// sorts pps job info by creation time, ascending.
type ByCreationTime []*ppsclient.JobInfo

func (arr ByCreationTime) Len() int { return len(arr) }

func (arr ByCreationTime) Swap(i, j int) { arr[i], arr[j] = arr[j], arr[i] }

func (arr ByCreationTime) Less(i, j int) bool {
	if arr[i].Started == nil || arr[j].Started == nil {
		return false
	}

	if arr[i].Started.Seconds < arr[j].Started.Seconds {
		return true
	} else if arr[i].Started.Seconds == arr[j].Started.Seconds {
		return arr[i].Started.Nanos < arr[j].Started.Nanos
	}

	return false
}

// pipelineManifestReader helps with unmarshalling pipeline configs from JSON. It's used by
// create-pipeline and update-pipeline
type pipelineManifestReader struct {
	buf     bytes.Buffer
	decoder *json.Decoder
}

func newPipelineManifestReader(path string) (result *pipelineManifestReader, retErr error) {
	result = new(pipelineManifestReader)
	var pipelineReader io.Reader
	if path == "-" {
		pipelineReader = io.TeeReader(os.Stdin, &result.buf)
		fmt.Print("Reading from stdin.\n")
	} else if url, err := url.Parse(path); err == nil && url.Scheme != "" {
		resp, err := http.Get(url.String())
		if err != nil {
			return nil, sanitizeErr(err)
		}
		defer func() {
			if err := resp.Body.Close(); err != nil && retErr == nil {
				retErr = sanitizeErr(err)
			}
		}()
		rawBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		pipelineReader = io.TeeReader(strings.NewReader(string(rawBytes)), &result.buf)
	} else {
		rawBytes, err := ioutil.ReadFile(path)
		if err != nil {
			return nil, err
		}

		pipelineReader = io.TeeReader(strings.NewReader(string(rawBytes)), &result.buf)
	}
	result.decoder = json.NewDecoder(pipelineReader)
	return result, nil
}

func (r *pipelineManifestReader) nextCreatePipelineRequest() (*ppsclient.CreatePipelineRequest, error) {
	var result ppsclient.CreatePipelineRequest
	if err := jsonpb.UnmarshalNext(r.decoder, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func describeSyntaxError(originalErr error, parsedBuffer bytes.Buffer) error {

	sErr, ok := originalErr.(*json.SyntaxError)
	if !ok {
		return originalErr
	}

	buffer := make([]byte, sErr.Offset)
	parsedBuffer.Read(buffer)

	lineOffset := strings.LastIndex(string(buffer[:len(buffer)-1]), "\n")
	if lineOffset == -1 {
		lineOffset = 0
	}

	lines := strings.Split(string(buffer[:len(buffer)-1]), "\n")
	lineNumber := len(lines)

	descriptiveErrorString := fmt.Sprintf("Syntax Error on line %v:\n%v\n%v^\n%v\n",
		lineNumber,
		string(buffer[lineOffset:]),
		strings.Repeat(" ", int(sErr.Offset)-2-lineOffset),
		originalErr,
	)

	return errors.New(descriptiveErrorString)
}

func sanitizeErr(err error) error {
	if err == nil {
		return nil
	}

	return errors.New(grpc.ErrorDesc(err))
}

// pushImage pushes an image as registry/user/image. Registry and user can be
// left empty.
func pushImage(registry string, username string, password string, image string) (string, error) {
	client, err := docker.NewClientFromEnv()
	if err != nil {
		return "", err
	}
	repo, _ := docker.ParseRepositoryTag(image)
	components := strings.Split(repo, "/")
	name := components[len(components)-1]
	if username == "" {
		user, err := user.Current()
		if err != nil {
			return "", err
		}
		username = user.Username
	}
	pushRepo := fmt.Sprintf("%s/%s/%s", registry, username, name)
	pushTag := uuid.NewWithoutDashes()
	if err := client.TagImage(image, docker.TagImageOptions{
		Repo:    pushRepo,
		Tag:     pushTag,
		Context: context.Background(),
	}); err != nil {
		return "", err
	}
	var authConfig docker.AuthConfiguration
	if password != "" {
		authConfig = docker.AuthConfiguration{ServerAddress: registry}
		authConfig.Username = username
		authConfig.Password = password
	} else {
		authConfigs, err := docker.NewAuthConfigurationsFromDockerCfg()
		if err != nil {
			return "", fmt.Errorf("error parsing auth: %s, try running `docker login`", err.Error())
		}
		for _, _authConfig := range authConfigs.Configs {
			serverAddress := _authConfig.ServerAddress
			if strings.Contains(serverAddress, registry) {
				authConfig = _authConfig
				break
			}
		}
	}
	fmt.Printf("Pushing %s:%s, this may take a while.\n", pushRepo, pushTag)
	if err := client.PushImage(
		docker.PushImageOptions{
			Name: pushRepo,
			Tag:  pushTag,
		},
		authConfig,
	); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%s", pushRepo, pushTag), nil
}
