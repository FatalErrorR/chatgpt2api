"""A minimal HTTP CONNECT proxy for the walkthrough: the "good" proxies in the check-and-retire flow.

The proxy check fetches https://chatgpt.com/api/auth/csrf through each proxy with certificate checks
on, so a passing proxy has to really reach chatgpt.com; this one tunnels to it directly. Credentials are
ignored, so http://anyname:pw@127.0.0.1:<port> are distinct proxies that all work.
usage: python3 tunnel.py <port>
"""
import asyncio
import sys


async def pipe(reader, writer):
    try:
        while data := await reader.read(65536):
            writer.write(data)
            await writer.drain()
    except (ConnectionError, asyncio.CancelledError):
        pass
    finally:
        writer.close()


async def handle(reader, writer):
    try:
        head = await reader.readuntil(b"\r\n\r\n")
        method, target, _ = head.split(b"\r\n", 1)[0].decode().split(" ", 2)
        if method != "CONNECT":
            writer.write(b"HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\n\r\n")
            return writer.close()
        host, port = target.rsplit(":", 1)
        up_reader, up_writer = await asyncio.wait_for(asyncio.open_connection(host, int(port)), 10)
    except Exception:
        writer.write(b"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
        return writer.close()
    writer.write(b"HTTP/1.1 200 Connection Established\r\n\r\n")
    await writer.drain()
    await asyncio.gather(pipe(reader, up_writer), pipe(up_reader, writer))


async def main(port):
    server = await asyncio.start_server(handle, "127.0.0.1", port)
    async with server:
        await server.serve_forever()


asyncio.run(main(int(sys.argv[1])))
