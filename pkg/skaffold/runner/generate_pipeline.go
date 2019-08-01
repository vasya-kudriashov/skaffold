/*
Copyright 2019 The Skaffold Authors

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

package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/pkg/errors"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/color"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/latest"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/version"

	tekton "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1alpha1"
	yamlv2 "gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	// for testing
	reader = bufio.NewReader(os.Stdin)
)

func (r *SkaffoldRunner) GeneratePipeline(ctx context.Context, out io.Writer, config *latest.SkaffoldConfig, fileOut string) error {
	err := createSkaffoldProfile(out, config, r.runCtx.Opts.ConfigurationFile)
	if err != nil {
		return errors.Wrap(err, "setting up profile")
	}

	color.Default.Fprintln(out, "Generating Pipeline...")

	// Generate git resource for pipeline
	gitResource, err := generateGitResource()
	if err != nil {
		return errors.Wrap(err, "generating git resource for pipeline")
	}

	// Generate build task for pipeline
	var tasks []*tekton.Task
	taskBuild, err := generateBuildTask(config.Pipeline.Build)
	if err != nil {
		return errors.Wrap(err, "generating build task")
	}
	tasks = append(tasks, taskBuild)

	// Generate deploy task for pipeline
	taskDeploy, err := generateDeployTask(config.Pipeline.Deploy)
	if err != nil {
		return errors.Wrap(err, "generating deploy task")
	}
	tasks = append(tasks, taskDeploy)

	// Generate pipeline from git resource and tasks
	pipeline, err := generatePipeline(tasks)
	if err != nil {
		return errors.Wrap(err, "generating tekton pipeline")
	}

	// json.Marshal all pieces of pipeline, then convert all jsons to yamls
	var jsons [][]byte
	bGitResource, err := json.Marshal(gitResource)
	if err != nil {
		return errors.Wrap(err, "marshaling git resource")
	}
	jsons = append(jsons, bGitResource)
	for _, task := range tasks {
		bTask, err := json.Marshal(task)
		if err != nil {
			return errors.Wrap(err, "marshaling task")
		}
		jsons = append(jsons, bTask)
	}
	bPipeline, err := json.Marshal(pipeline)
	if err != nil {
		return errors.Wrap(err, "marshaling pipeline")
	}
	jsons = append(jsons, bPipeline)

	var output bytes.Buffer
	for _, item := range jsons {
		itemYaml, err := yaml.JSONToYAML(item)
		if err != nil {
			return errors.Wrap(err, "converting jsons to yamls")
		}
		output.Write(append(itemYaml, []byte("---\n")...))
	}

	// write all yaml pieces to output
	return ioutil.WriteFile(fileOut, output.Bytes(), 0755)
}

func generateGitResource() (*tekton.PipelineResource, error) {
	// Get git repo url
	gitRepo := os.Getenv("SKAFFOLD_GIT_URL")
	if gitRepo == "" {
		getGitRepo := exec.Command("git", "config", "--get", "remote.origin.url")
		bGitRepo, err := getGitRepo.Output()
		if err != nil {
			return nil, errors.Wrap(err, "getting git repo from git config")
		}
		gitRepo = string(bGitRepo)
	}

	// Create git resource for pipeline from users current git repo
	return &tekton.PipelineResource{
		TypeMeta: metav1.TypeMeta{
			Kind:       "PipelineResource",
			APIVersion: "tekton.dev/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "source-git",
		},
		Spec: tekton.PipelineResourceSpec{
			Type: tekton.PipelineResourceTypeGit,
			Params: []tekton.ResourceParam{
				{
					Name:  "url",
					Value: gitRepo,
				},
			},
		},
	}, nil
}

func generateBuildTask(buildConfig latest.BuildConfig) (*tekton.Task, error) {
	if len(buildConfig.Artifacts) == 0 {
		return nil, errors.New("no artifacts to build")
	}

	return &tekton.Task{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Task",
			APIVersion: "tekton.dev/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "skaffold-build",
		},
		Spec: tekton.TaskSpec{
			Inputs: &tekton.Inputs{
				Resources: []tekton.TaskResource{
					{
						Name: "source",
						Type: tekton.PipelineResourceTypeGit,
					},
				},
			},
			Steps: []corev1.Container{
				{
					Name:       "run-build",
					Image:      fmt.Sprintf("gcr.io/k8s-skaffold/skaffold:%s", version.Get().Version),
					WorkingDir: "/workspace/source",
					Command:    []string{"skaffold"},
					Args: []string{"build",
						"--filename", "skaffold.yaml",
						"--profile", "oncluster",
						"--file-output", "build.out",
					},
				},
			},
		},
	}, nil
}

func generateDeployTask(deployConfig latest.DeployConfig) (*tekton.Task, error) {
	if deployConfig.HelmDeploy == nil && deployConfig.KubectlDeploy == nil && deployConfig.KustomizeDeploy == nil {
		return nil, errors.New("no Helm/Kubectl/Kustomize deploy config")
	}

	return &tekton.Task{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Task",
			APIVersion: "tekton.dev/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "skaffold-deploy",
		},
		Spec: tekton.TaskSpec{
			Inputs: &tekton.Inputs{
				Resources: []tekton.TaskResource{
					{
						Name: "source",
						Type: tekton.PipelineResourceTypeGit,
					},
				},
			},
			Steps: []corev1.Container{
				{
					Name:       "run-deploy",
					Image:      fmt.Sprintf("gcr.io/k8s-skaffold/skaffold:%s", version.Get().Version),
					WorkingDir: "/workspace/source",
					Command:    []string{"skaffold"},
					Args: []string{
						"deploy",
						"--filename", "skaffold.yaml",
						"--profile", "oncluster",
						"--build-artifacts", "build.out",
					},
				},
			},
		},
	}, nil
}

func generatePipeline(tasks []*tekton.Task) (*tekton.Pipeline, error) {
	if len(tasks) == 0 {
		return nil, errors.New("no tasks to add to pipeline")
	}

	// Create pipeline to tie together all tasks
	pipeline := &tekton.Pipeline{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pipeline",
			APIVersion: "tekton.dev/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "skaffold-pipeline",
		},
		Spec: tekton.PipelineSpec{
			Resources: []tekton.PipelineDeclaredResource{
				{
					Name: "source-repo",
					Type: tekton.PipelineResourceTypeGit,
				},
			},
			Tasks: []tekton.PipelineTask{},
		},
	}
	// Create tasks in pipeline spec for all corresponding tasks
	for i, task := range tasks {
		pipelineTask := tekton.PipelineTask{
			Name: fmt.Sprintf("%s-task", task.Name),
			TaskRef: tekton.TaskRef{
				Name: task.Name,
			},
			RunAfter: []string{},
			Resources: &tekton.PipelineTaskResources{
				Inputs: []tekton.PipelineTaskInputResource{
					{
						Name:     "source",
						Resource: "source-repo",
					},
				},
			},
		}
		if i > 0 {
			pipelineTask.RunAfter = []string{pipeline.Spec.Tasks[i-1].Name}
		}
		pipeline.Spec.Tasks = append(pipeline.Spec.Tasks, pipelineTask)
	}

	return pipeline, nil
}

func createSkaffoldProfile(out io.Writer, config *latest.SkaffoldConfig, configFile string) error {
	color.Default.Fprintln(out, "Checking for oncluster skaffold profile...")
	profileExists := false
	for _, profile := range config.Profiles {
		if profile.Name == "oncluster" {
			profileExists = true
			break
		}
	}

	// Check for existing oncluster profile, if none exists then prompt to create one
	if profileExists {
		color.Default.Fprintln(out, "profile \"oncluster\" found!")
		return nil
	}

confirmLoop:
	for {
		color.Default.Fprintf(out, "No profile \"oncluster\" found. Create one? [y/n]: ")
		response, err := reader.ReadString('\n')
		if err != nil {
			return errors.Wrap(err, "reading user confirmation")
		}

		response = strings.ToLower(strings.TrimSpace(response))
		switch response {
		case "y", "yes":
			break confirmLoop
		case "n", "no":
			return nil
		}
	}

	color.Default.Fprintln(out, "Creating skaffold profile \"oncluster\"...")
	profile, err := generateProfile(out, config)
	if err != nil {
		return errors.Wrap(err, "generating profile \"oncluster\"")
	}

	bProfile, err := yamlv2.Marshal([]*latest.Profile{profile})
	if err != nil {
		return errors.Wrap(err, "marshaling new profile")
	}

	fileContents, err := ioutil.ReadFile(configFile)
	if err != nil {
		return errors.Wrap(err, "reading file contents")
	}
	fileStrings := strings.Split(strings.TrimSpace(string(fileContents)), "\n")

	var profilePos int
	if len(config.Profiles) == 0 {
		// Create new profiles section
		fileStrings = append(fileStrings, "profiles:")
		profilePos = len(fileStrings)
	} else {
		for i, line := range fileStrings {
			if line == "profiles:" {
				profilePos = i + 1
			}
		}
	}

	fileStrings = append(fileStrings, "")
	copy(fileStrings[profilePos+1:], fileStrings[profilePos:])
	fileStrings[profilePos] = strings.TrimSpace(string(bProfile))

	fileContents = []byte((strings.Join(fileStrings, "\n")))

	if err := ioutil.WriteFile(configFile, fileContents, 0644); err != nil {
		return errors.Wrap(err, "writing profile to skaffold config")
	}

	return nil
}

func generateProfile(out io.Writer, config *latest.SkaffoldConfig) (*latest.Profile, error) {
	if len(config.Build.Artifacts) == 0 {
		return nil, errors.New("No Artifacts to add to profile")
	}

	profile := &latest.Profile{
		Name: "oncluster",
		Pipeline: latest.Pipeline{
			Build:  config.Pipeline.Build,
			Deploy: latest.DeployConfig{},
		},
	}
	profile.Build.Cluster = &latest.ClusterDetails{
		PullSecretName: "kaniko-secret",
	}
	profile.Build.LocalBuild = nil
	// Add kaniko build config for artifacts
	for _, artifact := range profile.Build.Artifacts {
		artifact.ImageName = fmt.Sprintf("%s-pipeline", artifact.ImageName)
		if artifact.DockerArtifact != nil {
			color.Default.Fprintf(out, "Cannot use Docker to build %s on cluster. Adding config for building with Kaniko.\n", artifact.ImageName)
			artifact.DockerArtifact = nil
			artifact.KanikoArtifact = &latest.KanikoArtifact{
				BuildContext: &latest.KanikoBuildContext{
					GCSBucket: "skaffold-kaniko",
				},
			}
		}
	}

	return profile, nil
}
