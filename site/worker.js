// Redirect www to the apex; serve everything else from static assets.
export default {
  fetch(req, env) {
    const url = new URL(req.url);
    if (url.hostname === "www.getfcp.com") {
      url.hostname = "getfcp.com";
      return Response.redirect(url.toString(), 301);
    }
    return env.ASSETS.fetch(req);
  },
};
