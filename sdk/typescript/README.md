# @applab/sdk@0.1.0

A TypeScript SDK client for the  API.

## Usage

First, install the SDK from npm.

```bash
npm install @applab/sdk --save
```

Next, try it out.


```ts
import {
  Configuration,
  AppsApi,
} from '@applab/sdk';
import type { CreateAppOperationRequest } from '@applab/sdk';

async function example() {
  console.log("🚀 Testing @applab/sdk SDK...");
  const config = new Configuration({ 
    // Configure HTTP bearer authorization: bearerAuth
    accessToken: "YOUR BEARER TOKEN",
  });
  const api = new AppsApi(config);

  const body = {
    // CreateAppRequest
    createAppRequest: ...,
  } satisfies CreateAppOperationRequest;

  try {
    const data = await api.createApp(body);
    console.log(data);
  } catch (error) {
    console.error(error);
  }
}

// Run the test
example().catch(console.error);
```


## Documentation

### API Endpoints

All URIs are relative to *http://http:*

| Class | Method | HTTP request | Description
| ----- | ------ | ------------ | -------------
*AppsApi* | [**createApp**](docs/AppsApi.md#createappoperation) | **POST** /api/v1/apps | Create an app.
*AppsApi* | [**deleteApp**](docs/AppsApi.md#deleteapp) | **DELETE** /api/v1/apps/{app} | Delete the app and everything applab recorded for it.
*AppsApi* | [**getApp**](docs/AppsApi.md#getapp) | **GET** /api/v1/apps/{app} | One app.
*AppsApi* | [**listApps**](docs/AppsApi.md#listapps) | **GET** /api/v1/apps | List apps.
*AppsApi* | [**updateApp**](docs/AppsApi.md#updateappoperation) | **PATCH** /api/v1/apps/{app} | Change an app\&#39;s settings: &#x60;{name?, port?, replicas?, dockerfile?, domain?, auto_deploy?, branch?, resources?}&#x60;.
*BuildsApi* | [**cancelBuild**](docs/BuildsApi.md#cancelbuild) | **DELETE** /api/v1/apps/{app}/builds/{build} | Stop a build that has not finished, and mark it &#x60;cancelled&#x60;.
*BuildsApi* | [**getBuild**](docs/BuildsApi.md#getbuild) | **GET** /api/v1/apps/{app}/builds/{build} | One build, read from the build Job: its commit, branch, status, and the image if it succeeded.
*BuildsApi* | [**listBuilds**](docs/BuildsApi.md#listbuilds) | **GET** /api/v1/apps/{app}/builds | An app\&#39;s builds, newest first, read from the build Jobs.
*BuildsApi* | [**startBuild**](docs/BuildsApi.md#startbuild) | **POST** /api/v1/apps/{app}/builds | Build an image from a commit.
*BuildsApi* | [**streamBuildLogs**](docs/BuildsApi.md#streambuildlogs) | **GET** /api/v1/apps/{app}/builds/{build}/logs | The build\&#39;s log, as &#x60;text/plain&#x60;.
*ConfigApi* | [**deleteAppEnv**](docs/ConfigApi.md#deleteappenv) | **DELETE** /api/v1/apps/{app}/env/{name} | Remove one environment variable.
*ConfigApi* | [**deleteAppSecret**](docs/ConfigApi.md#deleteappsecret) | **DELETE** /api/v1/apps/{app}/secrets/{name} | Remove one secret.
*ConfigApi* | [**getAgentFile**](docs/ConfigApi.md#getagentfile) | **GET** /api/v1/apps/{app}/agent/files/{file} | One of the files applab keeps in this app\&#39;s source tree (&#x60;applab.sh&#x60;, &#x60;AGENT.md&#x60;), as &#x60;text/plain&#x60;.
*ConfigApi* | [**getAppConfig**](docs/ConfigApi.md#getappconfig) | **GET** /api/v1/apps/{app}/config | The app\&#39;s configuration: environment variables with their values, and the *names* of its secrets.
*ConfigApi* | [**getAppKey**](docs/ConfigApi.md#getappkey) | **GET** /api/v1/apps/{app}/key | The app\&#39;s API key, in full.
*ConfigApi* | [**listAgentFiles**](docs/ConfigApi.md#listagentfiles) | **GET** /api/v1/apps/{app}/agent/files | The names of the files applab keeps in this app\&#39;s source tree.
*ConfigApi* | [**rotateAppKey**](docs/ConfigApi.md#rotateappkey) | **POST** /api/v1/apps/{app}/key/rotate | Replace the app\&#39;s API key, invalidating the previous one immediately.
*ConfigApi* | [**setAppEnv**](docs/ConfigApi.md#setappenv) | **PUT** /api/v1/apps/{app}/env | Set environment variables.
*ConfigApi* | [**setAppSecrets**](docs/ConfigApi.md#setappsecrets) | **PUT** /api/v1/apps/{app}/secrets | Set secret values.
*DeployApi* | [**deployApp**](docs/DeployApi.md#deployapp) | **POST** /api/v1/apps/{app}/deploy | Deploy a commit and return the URL it is served at.
*DeployApi* | [**getAppStatus**](docs/DeployApi.md#getappstatus) | **GET** /api/v1/apps/{app}/status | An app\&#39;s live state, read from the cluster: whether a Deployment exists, its ready replicas, the image and the commit it was built from.
*DeployApi* | [**restartApp**](docs/DeployApi.md#restartapp) | **POST** /api/v1/apps/{app}/restart | Roll the running pods, keeping the same image.
*DeployApi* | [**rollbackApp**](docs/DeployApi.md#rollbackapp) | **POST** /api/v1/apps/{app}/rollback | Deploy an earlier commit.
*DeployApi* | [**stopApp**](docs/DeployApi.md#stopapp) | **POST** /api/v1/apps/{app}/stop | Stop the app by removing its Deployment, Service and Ingress.
*GitApi* | [**gitSmartHTTP**](docs/GitApi.md#gitsmarthttp) | **GET** /git/{repo} | Clone or push an app\&#39;s repository over git\&#39;s smart HTTP protocol.
*MetaApi* | [**getBootstrapFiles**](docs/MetaApi.md#getbootstrapfiles) | **GET** /bootstrap | What &#x60;GET /bootstrap/applab.sh&#x60; serves.
*MetaApi* | [**getBootstrapScript**](docs/MetaApi.md#getbootstrapscript) | **GET** /bootstrap/applab.sh | The &#x60;applab.sh&#x60; AppLab writes into every app\&#39;s repository, rendered with no app and no key in it, as &#x60;text/plain&#x60;.
*MetaApi* | [**getConfig**](docs/MetaApi.md#getconfig) | **GET** /api/v1/config | This deployment\&#39;s limits and capabilities.
*MetaApi* | [**getDescribe**](docs/MetaApi.md#getdescribe) | **GET** /api/v1/describe | Start here.
*MetaApi* | [**getHealth**](docs/MetaApi.md#gethealth) | **GET** /health | Liveness.
*MetaApi* | [**getMetrics**](docs/MetaApi.md#getmetrics) | **GET** /metrics | Prometheus metrics for this deployment, as text.
*MetaApi* | [**getVersion**](docs/MetaApi.md#getversion) | **GET** /api/v1/version | Build version and commit.
*ObserveApi* | [**diagnoseApp**](docs/ObserveApi.md#diagnoseapp) | **GET** /api/v1/apps/{app}/diagnose | Why the app is not working, in one call: pods, events and the relevant log, ordered so the most likely cause comes first.
*ObserveApi* | [**getAppUsage**](docs/ObserveApi.md#getappusage) | **GET** /api/v1/apps/{app}/resources | What the app is using and what it may use: the CPU and memory summed across its pods, read from the cluster\&#39;s metrics API, beside the requests and limits its running containers actually have.
*ObserveApi* | [**listAppEvents**](docs/ObserveApi.md#listappevents) | **GET** /api/v1/apps/{app}/events | Recent Kubernetes events for the app, warnings first, with a &#x60;warnings&#x60; count.
*ObserveApi* | [**listAppPods**](docs/ObserveApi.md#listapppods) | **GET** /api/v1/apps/{app}/pods | The app\&#39;s pods, newest first, with per-container state.
*ObserveApi* | [**streamAppLogs**](docs/ObserveApi.md#streamapplogs) | **GET** /api/v1/apps/{app}/logs | A pod\&#39;s log as &#x60;text/plain&#x60;.
*PlatformApi* | [**getOverview**](docs/PlatformApi.md#getoverview) | **GET** /api/v1/overview | The platform at a glance: app counts by status, build counts and the most recent builds across every app, whether the cluster is configured and reachable, and this deployment\&#39;s self-description.
*PlatformApi* | [**getSelfUsage**](docs/PlatformApi.md#getselfusage) | **GET** /api/v1/platform/resources | CPU and memory for each of AppLab\&#39;s own pods, keyed by pod name, with &#x60;available&#x60; saying whether the cluster reports metrics at all.
*PlatformApi* | [**listSelfEvents**](docs/PlatformApi.md#listselfevents) | **GET** /api/v1/platform/events | Kubernetes events concerning AppLab\&#39;s own objects, warnings first, with &#x60;count&#x60; and &#x60;warnings&#x60;.
*PlatformApi* | [**listSelfPods**](docs/PlatformApi.md#listselfpods) | **GET** /api/v1/platform/pods | AppLab\&#39;s own pods, newest first — the deployment that serves this API, not the apps it manages.
*PlatformApi* | [**streamSelfLogs**](docs/PlatformApi.md#streamselflogs) | **GET** /api/v1/platform/logs | AppLab\&#39;s own log as &#x60;text/plain&#x60;, following by default; &#x60;?follow&#x3D;false&#x60; returns what exists and closes.
*ServersApi* | [**createServerApp**](docs/ServersApi.md#createserverapp) | **POST** /api/v1/servers/{server}/apps | Create an app on one server.
*ServersApi* | [**deleteServerApp**](docs/ServersApi.md#deleteserverapp) | **DELETE** /api/v1/servers/{server}/apps/{app} | Delete an app on one server.
*ServersApi* | [**getServer**](docs/ServersApi.md#getserver) | **GET** /api/v1/servers/{server} | One server, without its key.
*ServersApi* | [**getServerApp**](docs/ServersApi.md#getserverapp) | **GET** /api/v1/servers/{server}/apps/{app} | One app on one server.
*ServersApi* | [**listServerApps**](docs/ServersApi.md#listserverapps) | **GET** /api/v1/servers/{server}/apps | List the apps on one server.
*ServersApi* | [**listServers**](docs/ServersApi.md#listservers) | **GET** /api/v1/servers | Every AppLab this deployment can manage: the built-in &#x60;local&#x60; entry first — this deployment itself — then each registered remote.
*ServersApi* | [**registerServer**](docs/ServersApi.md#registerserveroperation) | **POST** /api/v1/servers | Register a remote AppLab.
*ServersApi* | [**removeServer**](docs/ServersApi.md#removeserver) | **DELETE** /api/v1/servers/{server} | Forget a registration.
*SourceApi* | [**completeChunkedUpload**](docs/SourceApi.md#completechunkedupload) | **POST** /api/v1/apps/{app}/source/uploads/{upload}/complete | Assemble every part and commit the result.
*SourceApi* | [**getCommit**](docs/SourceApi.md#getcommit) | **GET** /api/v1/apps/{app}/commits/{sha} | One commit.
*SourceApi* | [**listBranches**](docs/SourceApi.md#listbranches) | **GET** /api/v1/apps/{app}/branches | The branches this app has source for, and which one is active.
*SourceApi* | [**listCommits**](docs/SourceApi.md#listcommits) | **GET** /api/v1/apps/{app}/commits | An app\&#39;s commit history, newest first, with the current tip.
*SourceApi* | [**startChunkedUpload**](docs/SourceApi.md#startchunkedupload) | **POST** /api/v1/apps/{app}/source/uploads | Begin a chunked upload for source too large for one request.
*SourceApi* | [**switchBranch**](docs/SourceApi.md#switchbranchoperation) | **PUT** /api/v1/apps/{app}/branch | Make a branch active and deploy it.
*SourceApi* | [**uploadSource**](docs/SourceApi.md#uploadsource) | **POST** /api/v1/apps/{app}/source | Upload source as a tar or tar.gz and commit it.
*SourceApi* | [**uploadSourcePart**](docs/SourceApi.md#uploadsourcepart) | **PUT** /api/v1/apps/{app}/source/uploads/{upload}/parts/{index} | Send one part.


### Models

- [AgentFile](docs/AgentFile.md)
- [AgentFilesResponse](docs/AgentFilesResponse.md)
- [App](docs/App.md)
- [AppConfig](docs/AppConfig.md)
- [AppKey](docs/AppKey.md)
- [AppResources](docs/AppResources.md)
- [AppUsage](docs/AppUsage.md)
- [BootstrapFilesResponse](docs/BootstrapFilesResponse.md)
- [Branches](docs/Branches.md)
- [Build](docs/Build.md)
- [BuildRequest](docs/BuildRequest.md)
- [ChunkedUploadStartRequest](docs/ChunkedUploadStartRequest.md)
- [ChunkedUploadStartResponse](docs/ChunkedUploadStartResponse.md)
- [Commit](docs/Commit.md)
- [CommitList](docs/CommitList.md)
- [Config](docs/Config.md)
- [CreateApp201Response](docs/CreateApp201Response.md)
- [CreateAppRequest](docs/CreateAppRequest.md)
- [DeleteApp200Response](docs/DeleteApp200Response.md)
- [DeleteApp200ResponseData](docs/DeleteApp200ResponseData.md)
- [DeletedResponse](docs/DeletedResponse.md)
- [DeployRequest](docs/DeployRequest.md)
- [DeployResult](docs/DeployResult.md)
- [DescribeAPI](docs/DescribeAPI.md)
- [DescribeResponse](docs/DescribeResponse.md)
- [DiagnoseApp200Response](docs/DiagnoseApp200Response.md)
- [DiagnoseApp200ResponseData](docs/DiagnoseApp200ResponseData.md)
- [Diagnosis](docs/Diagnosis.md)
- [Endpoint](docs/Endpoint.md)
- [Event](docs/Event.md)
- [EventList](docs/EventList.md)
- [GetAppConfig200Response](docs/GetAppConfig200Response.md)
- [GetAppConfig200ResponseData](docs/GetAppConfig200ResponseData.md)
- [GetAppKey200Response](docs/GetAppKey200Response.md)
- [GetAppKey200ResponseData](docs/GetAppKey200ResponseData.md)
- [GetAppStatus200Response](docs/GetAppStatus200Response.md)
- [GetAppStatus200ResponseData](docs/GetAppStatus200ResponseData.md)
- [GetAppUsage200Response](docs/GetAppUsage200Response.md)
- [GetAppUsage200ResponseData](docs/GetAppUsage200ResponseData.md)
- [GetCommit200Response](docs/GetCommit200Response.md)
- [GetCommit200ResponseData](docs/GetCommit200ResponseData.md)
- [GetConfig200Response](docs/GetConfig200Response.md)
- [GetConfig200ResponseData](docs/GetConfig200ResponseData.md)
- [GetDescribe200Response](docs/GetDescribe200Response.md)
- [GetDescribe200ResponseData](docs/GetDescribe200ResponseData.md)
- [GetDescribe200ResponseDataAccess](docs/GetDescribe200ResponseDataAccess.md)
- [GetDescribe200ResponseDataBuild](docs/GetDescribe200ResponseDataBuild.md)
- [GetDescribe200ResponseDataDeploy](docs/GetDescribe200ResponseDataDeploy.md)
- [GetDescribe200ResponseDataHowTo](docs/GetDescribe200ResponseDataHowTo.md)
- [GetHealth200Response](docs/GetHealth200Response.md)
- [GetOverview200Response](docs/GetOverview200Response.md)
- [GetOverview200ResponseData](docs/GetOverview200ResponseData.md)
- [GetVersion200Response](docs/GetVersion200Response.md)
- [GetVersion200ResponseData](docs/GetVersion200ResponseData.md)
- [HealthResponse](docs/HealthResponse.md)
- [ListAgentFiles200Response](docs/ListAgentFiles200Response.md)
- [ListAgentFiles200ResponseData](docs/ListAgentFiles200ResponseData.md)
- [ListAppEvents200Response](docs/ListAppEvents200Response.md)
- [ListAppEvents200ResponseData](docs/ListAppEvents200ResponseData.md)
- [ListAppPods200Response](docs/ListAppPods200Response.md)
- [ListAppPods200ResponseData](docs/ListAppPods200ResponseData.md)
- [ListApps200Response](docs/ListApps200Response.md)
- [ListApps200ResponseDataInner](docs/ListApps200ResponseDataInner.md)
- [ListBranches200Response](docs/ListBranches200Response.md)
- [ListBranches200ResponseData](docs/ListBranches200ResponseData.md)
- [ListBuilds200Response](docs/ListBuilds200Response.md)
- [ListBuilds200ResponseDataInner](docs/ListBuilds200ResponseDataInner.md)
- [ListCommits200Response](docs/ListCommits200Response.md)
- [ListCommits200ResponseData](docs/ListCommits200ResponseData.md)
- [ListServers200Response](docs/ListServers200Response.md)
- [ListServers200ResponseDataInner](docs/ListServers200ResponseDataInner.md)
- [ModelError](docs/ModelError.md)
- [Overview](docs/Overview.md)
- [OverviewApps](docs/OverviewApps.md)
- [OverviewBuilds](docs/OverviewBuilds.md)
- [OverviewCluster](docs/OverviewCluster.md)
- [Pod](docs/Pod.md)
- [PodList](docs/PodList.md)
- [PodUsage](docs/PodUsage.md)
- [RegisterServer201Response](docs/RegisterServer201Response.md)
- [RegisterServerRequest](docs/RegisterServerRequest.md)
- [RemoveServer200Response](docs/RemoveServer200Response.md)
- [RemoveServer200ResponseData](docs/RemoveServer200ResponseData.md)
- [RemovedResponse](docs/RemovedResponse.md)
- [ResourcesRequest](docs/ResourcesRequest.md)
- [RestartApp200Response](docs/RestartApp200Response.md)
- [RestartApp200ResponseData](docs/RestartApp200ResponseData.md)
- [RestartResponse](docs/RestartResponse.md)
- [RollbackRequest](docs/RollbackRequest.md)
- [Server](docs/Server.md)
- [SetEnvRequest](docs/SetEnvRequest.md)
- [SetSecretsRequest](docs/SetSecretsRequest.md)
- [StartBuild202Response](docs/StartBuild202Response.md)
- [StartChunkedUpload201Response](docs/StartChunkedUpload201Response.md)
- [StartChunkedUpload201ResponseData](docs/StartChunkedUpload201ResponseData.md)
- [Status](docs/Status.md)
- [StopApp200Response](docs/StopApp200Response.md)
- [StopApp200ResponseData](docs/StopApp200ResponseData.md)
- [StopResponse](docs/StopResponse.md)
- [SwitchBranch200Response](docs/SwitchBranch200Response.md)
- [SwitchBranch200ResponseData](docs/SwitchBranch200ResponseData.md)
- [SwitchBranchRequest](docs/SwitchBranchRequest.md)
- [UpdateAppRequest](docs/UpdateAppRequest.md)
- [UploadPartResponse](docs/UploadPartResponse.md)
- [UploadResult](docs/UploadResult.md)
- [UploadSource200Response](docs/UploadSource200Response.md)
- [UploadSource200ResponseData](docs/UploadSource200ResponseData.md)
- [UploadSourcePart200Response](docs/UploadSourcePart200Response.md)
- [UploadSourcePart200ResponseData](docs/UploadSourcePart200ResponseData.md)
- [VersionResponse](docs/VersionResponse.md)

### Authorization


Authentication schemes defined for the API:
<a id="basicAuth"></a>
#### basicAuth


- **Type**: HTTP basic authentication
<a id="bearerAuth"></a>
#### bearerAuth


- **Type**: HTTP Bearer Token authentication

## About

This TypeScript SDK client supports the [Fetch API](https://fetch.spec.whatwg.org/)
and is automatically generated by the
[OpenAPI Generator](https://openapi-generator.tech) project:

- API version: `v1`
- Package version: `0.1.0`
- Generator version: `7.25.0`
- Build package: `org.openapitools.codegen.languages.TypeScriptFetchClientCodegen`

The generated npm module supports the following:

- Environments
  * Node.js
  * Webpack
  * Browserify
- Language levels
  * ES5 - you must have a Promises/A+ library installed
  * ES6
- Module systems
  * CommonJS
  * ES6 module system


## Development

### Building

To build the TypeScript source code, you need to have Node.js and npm installed.
After cloning the repository, navigate to the project directory and run:

```bash
npm install
npm run build
```

### Publishing

Once you've built the package, you can publish it to npm:

```bash
npm publish
```

## License

[]()
