// Runtime configuration. Replaced by a ConfigMap-mounted file in the cluster,
// so one built image runs anywhere. Note the absence of any satellite URL: the
// shell discovers those from the registry.
window.__DOCLINK_CONFIG__ = {
  doclink: "http://localhost:8080",
  pim: "http://localhost:8081",
};
