"""Allow API path redirects while keeping credentials and bodies at one origin."""
import aiohttp
from yarl import URL


def session(**kwargs):
    trace = aiohttp.TraceConfig()

    async def redirect(client, context, params):
        context.redirects = getattr(context, 'redirects', 0) + 1
        if context.redirects >= 10:
            raise aiohttp.ClientError('Too many API redirects')
        try:
            destination = params.url.join(URL(params.response.headers.get('Location', '')))
            allowed = destination.user is None and destination.origin() == params.url.origin()
        except (ValueError, TypeError):
            allowed = False
        if not allowed:
            # Keep the Location out of errors: it may contain private values.
            raise aiohttp.ClientError('API redirect outside configured origin refused') from None

    trace.on_request_redirect.append(redirect)
    return aiohttp.ClientSession(trace_configs=[trace], **kwargs)
