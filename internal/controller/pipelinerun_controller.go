// Copyright 2024 Red Hat, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"knative.dev/pkg/apis"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	tektonv1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"

	"github.com/konflux-ci/mintmaker/internal/pkg/config"
	. "github.com/konflux-ci/mintmaker/internal/pkg/constant"
	mintmakermetrics "github.com/konflux-ci/mintmaker/internal/pkg/metrics"
)

var (
	MintMakerGitPlatformLabel        = "mintmaker.appstudio.redhat.com/git-platform"
	MintMakerComponentNameLabel      = "mintmaker.appstudio.redhat.com/component"
	MintMakerComponentNamespaceLabel = "mintmaker.appstudio.redhat.com/namespace"
)

// PipelineRunReconciler reconciles a PipelineRun object
type PipelineRunReconciler struct {
	Client client.Client
	Scheme *runtime.Scheme
	Config *config.ControllerConfig
}

// updatePipelineRunState updates the status of a PipelineRun
func (r *PipelineRunReconciler) updatePipelineRunState(
	ctx context.Context,
	pipelineRun tektonv1.PipelineRun,
	status tektonv1.PipelineRunSpecStatus,
	errmsg string,
) error {
	log := ctrllog.FromContext(ctx)
	originalPipelineRun := pipelineRun.DeepCopy()
	pipelineRun.Spec.Status = status

	// If pipelinerun is to be cancelled, add reason with the error message
	if status == tektonv1.PipelineRunSpecStatusCancelled {
		pipelineRun.Status.MarkFailed(string(tektonv1.PipelineRunReasonCancelled), "%s", errmsg)
	}

	patch := client.MergeFrom(originalPipelineRun)
	if err := r.Client.Patch(ctx, &pipelineRun, patch); err != nil {
		log.Error(err, "unable to update pipelinerun status", "pipelinerun", pipelineRun.Name)
		return err
	}
	return nil
}

// startPipelineRun starts a pending PipelineRun by removing its pending status
// Returns true if successfully started, false otherwise
func (r *PipelineRunReconciler) startPipelineRun(ctx context.Context, plr tektonv1.PipelineRun) bool {
	log := ctrllog.FromContext(ctx)

	// Start the PipelineRun by removing the pending status
	log.Info("starting PipelineRun", "name", plr.Name)
	if err := r.updatePipelineRunState(ctx, plr, "", ""); err != nil {
		log.Error(err, "failed to start PipelineRun", "name", plr.Name)
		return false
	}

	return true
}

func (r *PipelineRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx).WithName("PipelineRunController")
	ctx = ctrllog.IntoContext(ctx, log)

	// Get all PipelineRuns in the namespace
	var pipelineRunList tektonv1.PipelineRunList
	if err := r.Client.List(ctx, &pipelineRunList, client.InNamespace(req.Namespace)); err != nil {
		log.Error(err, "unable to list PipelineRuns")
		return ctrl.Result{}, err
	}

	// Count running PipelineRuns
	runningCount := 0
	var pendingRuns []tektonv1.PipelineRun

	for i := range pipelineRunList.Items {
		run := pipelineRunList.Items[i]

		// Count running PipelineRuns - a running pipelinerun is one that is not pending and not done
		if !run.IsPending() && !run.IsDone() {
			runningCount++
		}

		// Collect pending PipelineRuns
		if run.IsPending() {
			pendingRuns = append(pendingRuns, run)
		}
	}

	// Sort pending runs by creation timestamp (oldest first)
	sort.Slice(pendingRuns, func(i, j int) bool {
		return pendingRuns[i].CreationTimestamp.Before(&pendingRuns[j].CreationTimestamp)
	})

	// Calculate how many more runs we can start
	availableSlots := r.Config.PipelineRunConfig.MaxParallelPipelineruns - runningCount

	// Start as many pending runs as possible, up to the maximum allowed
	if availableSlots > 0 {
		started := 0
		for i := 0; i < len(pendingRuns) && started < availableSlots; i++ {
			if r.startPipelineRun(ctx, pendingRuns[i]) {
				started++
				mintmakermetrics.CountScheduledRunSuccess()
			} else {
				mintmakermetrics.CountScheduledRunFailure()
			}
		}
		log.Info("started PipelineRuns", "count", started)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PipelineRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tektonv1.PipelineRun{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				if e.Object.GetNamespace() != MintMakerNamespaceName {
					return false
				}
				if pipelineRun, ok := e.Object.(*tektonv1.PipelineRun); ok {
					return pipelineRun.IsPending()
				}
				return false
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return false
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				if e.ObjectNew.GetNamespace() != MintMakerNamespaceName {
					return false
				}
				if oldPipelineRun, ok := e.ObjectOld.(*tektonv1.PipelineRun); ok {
					if newPipelineRun, ok := e.ObjectNew.(*tektonv1.PipelineRun); ok {
						if !oldPipelineRun.IsDone() && newPipelineRun.IsDone() {
							if newPipelineRun.Status.CompletionTime != nil {
								log := ctrl.Log.WithName("PipelineRunController")
								log.Info(
									fmt.Sprintf("PipelineRun finished: %s", newPipelineRun.Name),
									"completionTime",
									newPipelineRun.Status.CompletionTime.Format(time.RFC3339),
									"success",
									newPipelineRun.Status.GetCondition(apis.ConditionSucceeded).IsTrue(),
									"reason",
									newPipelineRun.Status.GetCondition(apis.ConditionSucceeded).GetReason(),
								)
							}
							return true
						}
					}
				}
				return false
			},
			GenericFunc: func(e event.GenericEvent) bool {
				return false
			},
		}).
		Complete(r)
}
