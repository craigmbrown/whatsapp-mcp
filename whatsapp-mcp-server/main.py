"""WhatsApp MCP server — wraps the local whatsapp-bridge HTTP API as MCP tools.

Canonical MCP stdio server using mcp.server.Server + stdio_server().
Rewritten 2026-05-13 to replace the broken OpenAI Agents SDK / TransportSelector
stub. Note: production WhatsApp ops go through the standalone bridge at port
8083 (see CLAUDE.md / feedback_whatsapp_p0_only); this MCP is a convenience
layer that talks to the local bridge HTTP API.
"""

import asyncio
import json
import logging
from typing import Any

from mcp.server import Server
from mcp.server.stdio import stdio_server
from mcp.types import TextContent, Tool

from whatsapp import (
    search_contacts as whatsapp_search_contacts,
    list_messages as whatsapp_list_messages,
    list_chats as whatsapp_list_chats,
    get_chat as whatsapp_get_chat,
    get_direct_chat_by_contact as whatsapp_get_direct_chat_by_contact,
    get_contact_chats as whatsapp_get_contact_chats,
    get_last_interaction as whatsapp_get_last_interaction,
    get_message_context as whatsapp_get_message_context,
    send_message as whatsapp_send_message,
    send_file as whatsapp_send_file,
    send_audio_message as whatsapp_audio_voice_message,
    download_media as whatsapp_download_media,
)

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("whatsapp-mcp")

server = Server("whatsapp")


def _json_default(value: Any) -> Any:
    """Convert datetime / dataclass-ish objects to JSON-safe form."""
    if hasattr(value, "isoformat"):
        return value.isoformat()
    if hasattr(value, "__dict__"):
        return value.__dict__
    return str(value)


def _to_text(payload: Any) -> list[TextContent]:
    return [TextContent(type="text", text=json.dumps(payload, indent=2, default=_json_default))]


@server.list_tools()
async def list_tools() -> list[Tool]:
    return [
        Tool(
            name="search_contacts",
            description="Search WhatsApp contacts by name or phone number.",
            inputSchema={
                "type": "object",
                "properties": {"query": {"type": "string"}},
                "required": ["query"],
            },
        ),
        Tool(
            name="list_messages",
            description="Get WhatsApp messages matching criteria, with optional context window.",
            inputSchema={
                "type": "object",
                "properties": {
                    "after": {"type": "string"},
                    "before": {"type": "string"},
                    "sender_phone_number": {"type": "string"},
                    "chat_jid": {"type": "string"},
                    "query": {"type": "string"},
                    "limit": {"type": "integer", "default": 20},
                    "page": {"type": "integer", "default": 0},
                    "include_context": {"type": "boolean", "default": True},
                    "context_before": {"type": "integer", "default": 1},
                    "context_after": {"type": "integer", "default": 1},
                },
            },
        ),
        Tool(
            name="list_chats",
            description="Get WhatsApp chats matching specified criteria.",
            inputSchema={
                "type": "object",
                "properties": {
                    "query": {"type": "string"},
                    "limit": {"type": "integer", "default": 20},
                    "page": {"type": "integer", "default": 0},
                    "include_last_message": {"type": "boolean", "default": True},
                    "sort_by": {"type": "string", "default": "last_active"},
                },
            },
        ),
        Tool(
            name="get_chat",
            description="Get WhatsApp chat metadata by JID.",
            inputSchema={
                "type": "object",
                "properties": {
                    "chat_jid": {"type": "string"},
                    "include_last_message": {"type": "boolean", "default": True},
                },
                "required": ["chat_jid"],
            },
        ),
        Tool(
            name="get_direct_chat_by_contact",
            description="Get WhatsApp chat metadata by sender phone number.",
            inputSchema={
                "type": "object",
                "properties": {"sender_phone_number": {"type": "string"}},
                "required": ["sender_phone_number"],
            },
        ),
        Tool(
            name="get_contact_chats",
            description="Get all WhatsApp chats involving a contact JID.",
            inputSchema={
                "type": "object",
                "properties": {
                    "jid": {"type": "string"},
                    "limit": {"type": "integer", "default": 20},
                    "page": {"type": "integer", "default": 0},
                },
                "required": ["jid"],
            },
        ),
        Tool(
            name="get_last_interaction",
            description="Get the most recent message involving a contact.",
            inputSchema={
                "type": "object",
                "properties": {"jid": {"type": "string"}},
                "required": ["jid"],
            },
        ),
        Tool(
            name="get_message_context",
            description="Get N messages before/after a specific message by ID.",
            inputSchema={
                "type": "object",
                "properties": {
                    "message_id": {"type": "string"},
                    "before": {"type": "integer", "default": 5},
                    "after": {"type": "integer", "default": 5},
                },
                "required": ["message_id"],
            },
        ),
        Tool(
            name="send_message",
            description="Send a WhatsApp text message.",
            inputSchema={
                "type": "object",
                "properties": {
                    "recipient": {"type": "string"},
                    "message": {"type": "string"},
                },
                "required": ["recipient", "message"],
            },
        ),
        Tool(
            name="send_file",
            description="Send a file (image, document, etc.) via WhatsApp.",
            inputSchema={
                "type": "object",
                "properties": {
                    "recipient": {"type": "string"},
                    "media_path": {"type": "string"},
                },
                "required": ["recipient", "media_path"],
            },
        ),
        Tool(
            name="send_audio_message",
            description="Send a file as a WhatsApp voice/audio message.",
            inputSchema={
                "type": "object",
                "properties": {
                    "recipient": {"type": "string"},
                    "media_path": {"type": "string"},
                },
                "required": ["recipient", "media_path"],
            },
        ),
        Tool(
            name="download_media",
            description="Download media attached to a WhatsApp message by message_id + chat_jid.",
            inputSchema={
                "type": "object",
                "properties": {
                    "message_id": {"type": "string"},
                    "chat_jid": {"type": "string"},
                },
                "required": ["message_id", "chat_jid"],
            },
        ),
    ]


@server.call_tool()
async def call_tool(name: str, arguments: dict) -> list[TextContent]:
    logger.info("Tool call: %s", name)
    try:
        if name == "search_contacts":
            return _to_text({"contacts": await asyncio.to_thread(whatsapp_search_contacts, arguments["query"])})
        if name == "list_messages":
            return _to_text({
                "messages": await asyncio.to_thread(
                    whatsapp_list_messages,
                    after=arguments.get("after"),
                    before=arguments.get("before"),
                    sender_phone_number=arguments.get("sender_phone_number"),
                    chat_jid=arguments.get("chat_jid"),
                    query=arguments.get("query"),
                    limit=arguments.get("limit", 20),
                    page=arguments.get("page", 0),
                    include_context=arguments.get("include_context", True),
                    context_before=arguments.get("context_before", 1),
                    context_after=arguments.get("context_after", 1),
                )
            })
        if name == "list_chats":
            return _to_text({
                "chats": await asyncio.to_thread(
                    whatsapp_list_chats,
                    query=arguments.get("query"),
                    limit=arguments.get("limit", 20),
                    page=arguments.get("page", 0),
                    include_last_message=arguments.get("include_last_message", True),
                    sort_by=arguments.get("sort_by", "last_active"),
                )
            })
        if name == "get_chat":
            return _to_text({"chat": await asyncio.to_thread(
                whatsapp_get_chat, arguments["chat_jid"], arguments.get("include_last_message", True)
            )})
        if name == "get_direct_chat_by_contact":
            return _to_text({"chat": await asyncio.to_thread(
                whatsapp_get_direct_chat_by_contact, arguments["sender_phone_number"]
            )})
        if name == "get_contact_chats":
            return _to_text({"chats": await asyncio.to_thread(
                whatsapp_get_contact_chats,
                arguments["jid"],
                arguments.get("limit", 20),
                arguments.get("page", 0),
            )})
        if name == "get_last_interaction":
            return _to_text({"message": await asyncio.to_thread(
                whatsapp_get_last_interaction, arguments["jid"]
            )})
        if name == "get_message_context":
            return _to_text({"context": await asyncio.to_thread(
                whatsapp_get_message_context,
                arguments["message_id"],
                arguments.get("before", 5),
                arguments.get("after", 5),
            )})
        if name == "send_message":
            success, status_message = await asyncio.to_thread(
                whatsapp_send_message, arguments["recipient"], arguments["message"]
            )
            return _to_text({"success": success, "message": status_message})
        if name == "send_file":
            success, status_message = await asyncio.to_thread(
                whatsapp_send_file, arguments["recipient"], arguments["media_path"]
            )
            return _to_text({"success": success, "message": status_message})
        if name == "send_audio_message":
            success, status_message = await asyncio.to_thread(
                whatsapp_audio_voice_message, arguments["recipient"], arguments["media_path"]
            )
            return _to_text({"success": success, "message": status_message})
        if name == "download_media":
            file_path = await asyncio.to_thread(
                whatsapp_download_media, arguments["message_id"], arguments["chat_jid"]
            )
            return _to_text({"success": bool(file_path), "file_path": file_path})

        return _to_text({"error": f"Unknown tool: {name}"})
    except Exception as exc:
        logger.exception("Tool error: %s", name)
        return _to_text({"error": str(exc)})


async def serve() -> None:
    logger.info("WhatsApp MCP server starting (stdio)")
    async with stdio_server() as (read_stream, write_stream):
        await server.run(
            read_stream,
            write_stream,
            server.create_initialization_options(),
        )


def main() -> None:
    asyncio.run(serve())


if __name__ == "__main__":
    main()
