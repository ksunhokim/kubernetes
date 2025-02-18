/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package logs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	cmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"k8s.io/kubernetes/pkg/kubectl/polymorphichelpers"
	"k8s.io/kubernetes/pkg/kubectl/scheme"
	"k8s.io/kubernetes/pkg/kubectl/util"
	"k8s.io/kubernetes/pkg/kubectl/util/i18n"
	"k8s.io/kubernetes/pkg/kubectl/util/templates"
)

const (
	logsUsageStr = "logs [-f] [-p] (POD | TYPE/NAME) [-c CONTAINER]"
)

var (
	logsExample = templates.Examples(i18n.T(`
		# Return snapshot logs from pod nginx with only one container
		kubectl logs nginx

		# Return snapshot logs from pod nginx with multi containers
		kubectl logs nginx --all-containers=true

		# Return snapshot logs from all containers in pods defined by label app=nginx
		kubectl logs -lapp=nginx --all-containers=true

		# Return snapshot of previous terminated ruby container logs from pod web-1
		kubectl logs -p -c ruby web-1

		# Begin streaming the logs of the ruby container in pod web-1
		kubectl logs -f -c ruby web-1

		# Display only the most recent 20 lines of output in pod nginx
		kubectl logs --tail=20 nginx

		# Show all logs from pod nginx written in the last hour
		kubectl logs --since=1h nginx

		# Return snapshot logs from first container of a job named hello
		kubectl logs job/hello

		# Return snapshot logs from container nginx-1 of a deployment named nginx
		kubectl logs deployment/nginx -c nginx-1`))

	selectorTail    int64 = 10
	logsUsageErrStr       = fmt.Sprintf("expected '%s'.\nPOD or TYPE/NAME is a required argument for the logs command", logsUsageStr)
)

const (
	defaultPodLogsTimeout = 20 * time.Second
)

type LogsOptions struct {
	Namespace     string
	ResourceArg   string
	AllContainers bool
	Options       runtime.Object
	Resources     []string

	ConsumeRequestFn func(*rest.Request, io.Writer) error

	// PodLogOptions
	SinceTime    string
	SinceSeconds time.Duration
	Follow       bool
	Previous     bool
	Timestamps   bool
	LimitBytes   int64
	Tail         int64
	Container    string

	// whether or not a container name was given via --container
	ContainerNameSpecified bool
	Selector               string

	Object           runtime.Object
	GetPodTimeout    time.Duration
	RESTClientGetter genericclioptions.RESTClientGetter
	LogsForObject    polymorphichelpers.LogsForObjectFunc

	genericclioptions.IOStreams
}

func NewLogsOptions(streams genericclioptions.IOStreams, allContainers bool) *LogsOptions {
	return &LogsOptions{
		IOStreams:     streams,
		AllContainers: allContainers,
		Tail:          -1,
	}
}

// NewCmdLogs creates a new pod logs command
func NewCmdLogs(f cmdutil.Factory, streams genericclioptions.IOStreams) *cobra.Command {
	o := NewLogsOptions(streams, false)

	cmd := &cobra.Command{
		Use:                   logsUsageStr,
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Print the logs for a container in a pod"),
		Long:                  "Print the logs for a container in a pod or specified resource. If the pod has only one container, the container name is optional.",
		Example:               logsExample,
		PreRun: func(cmd *cobra.Command, args []string) {
			if len(os.Args) > 1 && os.Args[1] == "log" {
				fmt.Fprintf(o.ErrOut, "%s is DEPRECATED and will be removed in a future version. Use %s instead.\n", "log", "logs")
			}
		},
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(f, cmd, args))
			cmdutil.CheckErr(o.Validate())
			cmdutil.CheckErr(o.RunLogs())
		},
		Aliases: []string{"log"},
	}
	cmd.Flags().BoolVar(&o.AllContainers, "all-containers", o.AllContainers, "Get all containers's logs in the pod(s).")
	cmd.Flags().BoolVarP(&o.Follow, "follow", "f", o.Follow, "Specify if the logs should be streamed.")
	cmd.Flags().BoolVar(&o.Timestamps, "timestamps", o.Timestamps, "Include timestamps on each line in the log output")
	cmd.Flags().Int64Var(&o.LimitBytes, "limit-bytes", o.LimitBytes, "Maximum bytes of logs to return. Defaults to no limit.")
	cmd.Flags().BoolVarP(&o.Previous, "previous", "p", o.Previous, "If true, print the logs for the previous instance of the container in a pod if it exists.")
	cmd.Flags().Int64Var(&o.Tail, "tail", o.Tail, "Lines of recent log file to display. Defaults to -1 with no selector, showing all log lines otherwise 10, if a selector is provided.")
	cmd.Flags().StringVar(&o.SinceTime, "since-time", o.SinceTime, i18n.T("Only return logs after a specific date (RFC3339). Defaults to all logs. Only one of since-time / since may be used."))
	cmd.Flags().DurationVar(&o.SinceSeconds, "since", o.SinceSeconds, "Only return logs newer than a relative duration like 5s, 2m, or 3h. Defaults to all logs. Only one of since-time / since may be used.")
	cmd.Flags().StringVarP(&o.Container, "container", "c", o.Container, "Print the logs of this container")
	cmdutil.AddPodRunningTimeoutFlag(cmd, defaultPodLogsTimeout)
	cmd.Flags().StringVarP(&o.Selector, "selector", "l", o.Selector, "Selector (label query) to filter on.")
	return cmd
}

func (o *LogsOptions) ToLogOptions() (*corev1.PodLogOptions, error) {
	logOptions := &corev1.PodLogOptions{
		Container:  o.Container,
		Follow:     o.Follow,
		Previous:   o.Previous,
		Timestamps: o.Timestamps,
	}

	if len(o.SinceTime) > 0 {
		t, err := util.ParseRFC3339(o.SinceTime, metav1.Now)
		if err != nil {
			return nil, err
		}

		logOptions.SinceTime = &t
	}

	if o.LimitBytes != 0 {
		logOptions.LimitBytes = &o.LimitBytes
	}

	if o.SinceSeconds != 0 {
		// round up to the nearest second
		sec := int64(o.SinceSeconds.Round(time.Second).Seconds())
		logOptions.SinceSeconds = &sec
	}

	if len(o.Selector) > 0 && o.Tail != -1 {
		logOptions.TailLines = &selectorTail
	} else if o.Tail != -1 {
		logOptions.TailLines = &o.Tail
	}

	return logOptions, nil
}

func (o *LogsOptions) Complete(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
	o.ContainerNameSpecified = cmd.Flag("container").Changed
	o.Resources = args

	switch len(args) {
	case 0:
		if len(o.Selector) == 0 {
			return cmdutil.UsageErrorf(cmd, "%s", logsUsageErrStr)
		}
	case 1:
		o.ResourceArg = args[0]
		if len(o.Selector) != 0 {
			return cmdutil.UsageErrorf(cmd, "only a selector (-l) or a POD name is allowed")
		}
	case 2:
		o.ResourceArg = args[0]
		o.Container = args[1]
	default:
		return cmdutil.UsageErrorf(cmd, "%s", logsUsageErrStr)
	}
	var err error
	o.Namespace, _, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}

	o.ConsumeRequestFn = DefaultConsumeRequest

	o.GetPodTimeout, err = cmdutil.GetPodRunningTimeoutFlag(cmd)
	if err != nil {
		return err
	}

	o.Options, err = o.ToLogOptions()
	if err != nil {
		return err
	}

	o.RESTClientGetter = f
	o.LogsForObject = polymorphichelpers.LogsForObjectFn

	if o.Object == nil {
		builder := f.NewBuilder().
			WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
			NamespaceParam(o.Namespace).DefaultNamespace().
			SingleResourceType()
		if o.ResourceArg != "" {
			builder.ResourceNames("pods", o.ResourceArg)
		}
		if o.Selector != "" {
			builder.ResourceTypes("pods").LabelSelectorParam(o.Selector)
		}
		infos, err := builder.Do().Infos()
		if err != nil {
			return err
		}
		if o.Selector == "" && len(infos) != 1 {
			return errors.New("expected a resource")
		}
		o.Object = infos[0].Object
	}

	return nil
}

func (o LogsOptions) Validate() error {
	if o.Follow && len(o.Selector) > 0 {
		return fmt.Errorf("only one of follow (-f) or selector (-l) is allowed")
	}

	if len(o.SinceTime) > 0 && o.SinceSeconds != 0 {
		return fmt.Errorf("at most one of `sinceTime` or `sinceSeconds` may be specified")
	}

	logsOptions, ok := o.Options.(*corev1.PodLogOptions)
	if !ok {
		return errors.New("unexpected logs options object")
	}
	if o.AllContainers && len(logsOptions.Container) > 0 {
		return fmt.Errorf("--all-containers=true should not be specified with container name %s", logsOptions.Container)
	}

	if o.ContainerNameSpecified && len(o.Resources) == 2 {
		return fmt.Errorf("only one of -c or an inline [CONTAINER] arg is allowed")
	}

	if o.LimitBytes < 0 {
		return fmt.Errorf("--limit-bytes must be greater than 0")
	}

	if logsOptions.SinceSeconds != nil && *logsOptions.SinceSeconds < int64(0) {
		return fmt.Errorf("--since must be greater than 0")
	}

	if logsOptions.TailLines != nil && *logsOptions.TailLines < 0 {
		return fmt.Errorf("tailLines must be greater than or equal to 0")
	}

	return nil
}

// RunLogs retrieves a pod log
func (o LogsOptions) RunLogs() error {
	requests, err := o.LogsForObject(o.RESTClientGetter, o.Object, o.Options, o.GetPodTimeout, o.AllContainers)
	if err != nil {
		return err
	}

	for _, request := range requests {
		if err := o.ConsumeRequestFn(request, o.Out); err != nil {
			return err
		}
	}

	return nil
}

func DefaultConsumeRequest(request *rest.Request, out io.Writer) error {
	readCloser, err := request.Stream()
	if err != nil {
		return err
	}
	defer readCloser.Close()

	_, err = io.Copy(out, readCloser)
	return err
}
