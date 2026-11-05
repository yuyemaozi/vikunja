// Vikunja is a to-do list application to facilitate your life.
// Copyright 2018-present Vikunja and contributors. All rights reserved.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package models

import (
	"fmt"

	"code.vikunja.io/api/pkg/user"
	"xorm.io/xorm"
)

// All reads happen in the original business transaction. A creator gets a deep
// copy with their own task/project state; committed bytes are never rebuilt.
func snapshotForWebhook(s *xorm.Session, common []byte, sourceProjectID int64, w *Webhook) (map[string]interface{}, bool, error) {
	creator, err := user.GetUserByIDForAuth(s, w.CreatedByID)
	if user.IsErrUserDoesNotExist(err) || user.IsErrUserStatusError(err) || user.IsErrEmailNotConfirmed(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if w.UserID > 0 && w.UserID != creator.ID {
		return nil, false, nil
	}
	event := map[string]interface{}{}
	if err := decodeEventPayload(common, &event); err != nil {
		return nil, false, err
	}
	if sourceProjectID > 0 {
		project, allowed, err := snapshotProjectForCreator(s, sourceProjectID, creator)
		if err != nil || !allowed {
			return nil, false, err
		}
		event["project"] = project
	}
	if id := getTaskIDFromEvent(event); id > 0 {
		task, allowed, err := snapshotTaskForCreator(s, id, creator)
		if err != nil || !allowed {
			return nil, false, err
		}
		event["task"] = task
	}
	if tasks, has := event["tasks"].([]interface{}); has {
		allowed, err := snapshotBatchForCreator(s, event, tasks, creator)
		if err != nil || !allowed {
			return nil, false, err
		}
	}
	return event, true, nil
}

func snapshotProjectForCreator(s *xorm.Session, id int64, creator *user.User) (*Project, bool, error) {
	project := &Project{ID: id}
	allowed, _, err := project.CanRead(s, creator)
	if err != nil || !allowed {
		return nil, false, err
	}
	if err := project.ReadOne(s, creator); err != nil {
		return nil, false, err
	}
	return project, true, nil
}

func snapshotTaskForCreator(s *xorm.Session, id int64, creator *user.User) (*Task, bool, error) {
	task := &Task{ID: id, Expand: []TaskCollectionExpandable{TaskCollectionExpandBuckets}}
	allowed, _, err := task.CanRead(s, creator)
	if err != nil || !allowed {
		return nil, false, err
	}
	if err := task.ReadOne(s, creator); err != nil {
		return nil, false, err
	}
	return task, true, nil
}

func snapshotBatchForCreator(s *xorm.Session, event map[string]interface{}, tasks []interface{}, creator *user.User) (bool, error) {
	frozen := make([]*Task, 0, len(tasks))
	projects := map[int64]*Project{}
	for _, value := range tasks {
		data, ok := value.(map[string]interface{})
		if !ok {
			return false, fmt.Errorf("invalid batch task snapshot")
		}
		id := getIDAsInt64(data["id"])
		if id <= 0 {
			return false, fmt.Errorf("invalid batch task id")
		}
		task, allowed, err := snapshotTaskForCreator(s, id, creator)
		if err != nil {
			return false, err
		}
		if !allowed {
			continue
		}
		project, allowed, err := snapshotProjectForCreator(s, task.ProjectID, creator)
		if err != nil {
			return false, err
		}
		if !allowed {
			continue
		}
		frozen = append(frozen, task)
		projects[project.ID] = project
	}
	if len(frozen) == 0 {
		return false, nil
	}
	event["tasks"] = frozen
	event["projects"] = projects
	return true, nil
}
