# Github Workflow Metrics

Pulls workflow timing information from the Github API for a given Workflow Id and pushes workflow / job / step timing
metrics to somewhere using remote_write.

## To run
Have the following in the environment:
* GITHUB_TOKEN
* GITHUB_OWNER
* GITHUB_REPO
* GITHUB_RUN_ID
* REMOTE_WRITE_URL
* REMOTE_WRITE_USERNAME
* REMOTE_WRITE_PASSWORD

